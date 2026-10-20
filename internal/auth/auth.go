// Package auth 解析 Qoder 账号凭证：磁盘形态、PAT 打包形式、地区判定、
// 原子写回与目录扫描。
//
// 一个账号一行 JSON（auths/qoder-<uid>.json）：
//
//	{"mode":"global","accessToken":"dt-…","refreshToken":"…|<uid>|<machineId>",
//	 "expiresAt":1792369369000,"userId":"…","email":"…","nickname":"…","machineId":"…"}
//
// 刷新串（refreshToken）的形态由上游决定，两种都可能出现：
//
//	pat|<PAT>|<job refresh>|<uid>|<machineId>   ← 用个人访问令牌换来的账号
//	<device refresh>|<uid>|<machineId>          ← 设备授权登录的账号
//
// 前缀与后缀都由这里解释，别的包只看 PAT()/OAuthRefresh()/CanRefresh()。
package auth

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// tmpSeq 让同一个账号文件的临时名在进程内唯一（见 saveAtomicLocked）。
var tmpSeq atomic.Uint64

// 地区（realm）：国际版与国内版是两套 host、两套凭证，互不通用。
const (
	RealmGlobal = "global"
	RealmCN     = "cn"
	// RealmIntl 是国际版的旧写法（面板/日志里两个名字都出现过，别再分叉第三个别名）。
	RealmIntl = RealmGlobal
)

// MaxTokenLength 是 token 字段长度上限（与上游客户端同一口径）。
const MaxTokenLength = 8192

// Auth 归一化后的账号凭证。
//
// 并发模型：mu 保护可变字段（AccessToken/RefreshToken/ExpiresAt/MachineID），
// 写路径（upstream 续期）持写锁整段执行，读路径持读锁取快照。
// 其余字段（RealmID/UID/Nickname/Email/FilePath）加载后不变，直接读。
type Auth struct {
	mu sync.RWMutex

	AccessToken  string // COSY 身份 JSON 里的 security_oauth_token
	RefreshToken string // 打包形态见包注释，每次续期轮换
	ExpiresAt    int64  // Unix 毫秒（access token 到期时刻；0 = 未知）
	RealmID      string // "global" | "cn"
	MachineID    string // cosy-machineid，账号稳定设备指纹
	UID          string
	Nickname     string
	Email        string
	FilePath     string // 落盘路径；续期后原子写回
}

// Lock / Unlock / RLock / RUnlock 供其他包在改写字段期间加锁。
func (a *Auth) Lock()    { a.mu.Lock() }
func (a *Auth) Unlock()  { a.mu.Unlock() }
func (a *Auth) RLock()   { a.mu.RLock() }
func (a *Auth) RUnlock() { a.mu.RUnlock() }

// JWT 返回当前 access token 的读锁快照。
func (a *Auth) JWT() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken
}

// RefreshTokenValue 返回当前刷新串的读锁快照。
func (a *Auth) RefreshTokenValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.RefreshToken
}

// ExpiresAtValue 读 access token 到期时刻（Unix 毫秒）。
func (a *Auth) ExpiresAtValue() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ExpiresAt
}

// MachineIDValue 读设备指纹的读锁快照。
func (a *Auth) MachineIDValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.MachineID
}

// Realm 返回地区标识（"global" / "cn"）。空值按国际版处理。
func (a *Auth) Realm() string {
	if a.RealmID == RealmCN {
		return RealmCN
	}
	return RealmGlobal
}

// NeedsRefresh 报告 token 是否将在 within 内过期（无 expiry 一律按需要刷新）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.NeedsRefreshLocked(within)
}

// NeedsRefreshLocked 是 NeedsRefresh 的持锁内部版本。
func (a *Auth) NeedsRefreshLocked(within time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).UnixMilli() >= a.ExpiresAt
}

// SetTokens 写入一组新凭证（调用方随后负责 SaveAtomic）。
func (a *Auth) SetTokens(access, refresh string, expiresAtMs int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.AccessToken = access
	if refresh != "" {
		a.RefreshToken = refresh
	}
	if expiresAtMs > 0 {
		a.ExpiresAt = expiresAtMs
	}
}

// SetMachineID 换一套设备指纹（调用方随后负责 SaveAtomic）。
func (a *Auth) SetMachineID(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.MachineID = id
}

// PAT 返回刷新串里的 PAT（若是 PAT 型账号）。
func (a *Auth) PAT() (string, bool) {
	refresh := a.RefreshTokenValue()
	if !strings.HasPrefix(refresh, "pat|") {
		return "", false
	}
	value := strings.SplitN(strings.TrimPrefix(refresh, "pat|"), "|", 2)[0]
	if value == "" {
		return "", false
	}
	return value, true
}

// OAuthRefresh 返回设备授权/作业令牌的刷新串；PAT 型账号为空。
func (a *Auth) OAuthRefresh() string {
	if _, ok := a.PAT(); ok {
		return ""
	}
	return strings.SplitN(a.RefreshTokenValue(), "|", 2)[0]
}

// CanRefresh 表示该账号具备可用的续期手段。
func (a *Auth) CanRefresh() bool {
	if _, ok := a.PAT(); ok {
		return true
	}
	return a.OAuthRefresh() != ""
}

// PackRefresh 把上游返回的刷新令牌打包成「令牌|uid|machineId」。
func PackRefresh(refresh, uid, machineID string) string {
	if refresh == "" {
		return ""
	}
	return refresh + "|" + uid + "|" + machineID
}

// PackPATRefresh 把 PAT 型账号的刷新串重新打包（PAT 与 job refresh 都要保留）。
func PackPATRefresh(pat, jobRefresh, uid, machineID string) string {
	return "pat|" + pat + "|" + jobRefresh + "|" + uid + "|" + machineID
}

// repackRefresh 把刷新串的后缀（uid|machineId）改成当前值，前缀原样保留。
func repackRefresh(refresh, uid, machineID string) string {
	if refresh == "" {
		return ""
	}
	parts := strings.Split(refresh, "|")
	prefix := refresh
	if len(parts) >= 3 {
		prefix = strings.Join(parts[:len(parts)-2], "|")
	}
	return prefix + "|" + uid + "|" + machineID
}

// CompleteIdentity 补全用户/设备标识并重写刷新串后缀。
func (a *Auth) CompleteIdentity(defaultMachineID string) error {
	if a.UID == "" {
		return fmt.Errorf("缺少 Qoder 用户标识，请重新登录或粘贴完整账号记录")
	}
	if err := validateIdentity(a.UID); err != nil {
		return err
	}
	if a.MachineID == "" {
		a.MachineID = defaultMachineID
	}
	if a.MachineID == "" {
		return fmt.Errorf("缺少 Qoder 设备标识，请重新登录")
	}
	if err := validateIdentity(a.MachineID); err != nil {
		return err
	}
	a.RefreshToken = repackRefresh(a.RefreshToken, a.UID, a.MachineID)
	if len(a.RefreshToken) > MaxTokenLength {
		return fmt.Errorf("Qoder 刷新凭证过长")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 磁盘解析

// record 是落盘与导入共用的字段集（键名与官方客户端/参考实现一致）。
type record struct {
	Mode         string          `json:"mode"`
	Edition      string          `json:"edition"`
	AccessToken  string          `json:"accessToken"`
	Access       string          `json:"access"`
	Token        string          `json:"token"`
	AccessSnake  string          `json:"access_token"`
	RefreshToken string          `json:"refreshToken"`
	Refresh      string          `json:"refresh"`
	RefreshSnake string          `json:"refresh_token"`
	PAT          string          `json:"pat"`
	PersonalTokn string          `json:"personalAccessToken"`
	ExpiresAt    json.RawMessage `json:"expiresAt"`
	Expires      json.RawMessage `json:"expires"`
	ExpiresSnake json.RawMessage `json:"expires_at"`
	ExpiresToken json.RawMessage `json:"tokenExpiresAt"`
	UserID       string          `json:"userId"`
	UserIDSnake  string          `json:"user_id"`
	UID          string          `json:"uid"`
	Email        string          `json:"email"`
	Nickname     string          `json:"nickname"`
	Name         string          `json:"name"`
	MachineID    string          `json:"machineId"`
	MachineSnake string          `json:"machine_id"`
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func stripBearer(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 7 && strings.EqualFold(value[:7], "Bearer ") {
		return strings.TrimSpace(value[7:])
	}
	return value
}

// Parse 解析账号内容：磁盘形态与面板粘贴形态共用（字段别名与 Bearer 前缀都吃）。
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var rec record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	access := stripBearer(firstNonEmpty(rec.AccessToken, rec.Access, rec.Token, rec.AccessSnake))
	refresh := firstNonEmpty(rec.RefreshToken, rec.Refresh, rec.RefreshSnake)
	pat := firstNonEmpty(rec.PAT, rec.PersonalTokn)
	if pat != "" && !strings.HasPrefix(refresh, "pat|") {
		// 只给 PAT 的账号：刷新串由上游兑换后补齐，这里先占位，CompleteIdentity 补后缀。
		refresh = "pat|" + pat
	}
	if access == "" && refresh == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken and refreshToken")
	}
	for _, value := range []string{access, refresh} {
		if len(value) > MaxTokenLength || hasControl(value) {
			return nil, fmt.Errorf("parse_error: token 过长或包含非法控制字符")
		}
	}
	uid := firstNonEmpty(rec.UserID, rec.UserIDSnake, rec.UID)
	machine := firstNonEmpty(rec.MachineID, rec.MachineSnake)
	// 刷新串里打包的 uid / machineId 优先（它们必须与刷新时一致）。
	parts := strings.Split(refresh, "|")
	var packedUser, packedMachine string
	switch {
	case len(parts) == 5 && parts[0] == "pat":
		packedUser, packedMachine = parts[3], parts[4]
	case len(parts) == 3:
		packedUser, packedMachine = parts[1], parts[2]
	}
	if uid != "" && packedUser != "" && uid != packedUser {
		return nil, fmt.Errorf("parse_error: userId 与刷新凭证里的用户不一致")
	}
	if machine != "" && packedMachine != "" && machine != packedMachine {
		return nil, fmt.Errorf("parse_error: machineId 与刷新凭证里的设备不一致")
	}
	if uid == "" {
		uid = packedUser
	}
	if machine == "" {
		machine = packedMachine
	}
	a := &Auth{
		AccessToken:  access,
		RefreshToken: refresh,
		RealmID:      realmOf(firstNonEmpty(rec.Mode, rec.Edition)),
		MachineID:    machine,
		UID:          uid,
		Nickname:     firstNonEmpty(rec.Nickname, rec.Name),
		Email:        strings.TrimSpace(rec.Email),
	}
	for _, raw := range []json.RawMessage{rec.ExpiresAt, rec.Expires, rec.ExpiresSnake, rec.ExpiresToken} {
		if ms := timestampMillis(raw); ms != nil {
			a.ExpiresAt = *ms
			break
		}
	}
	for _, value := range []string{a.UID, a.MachineID} {
		if value != "" {
			if err := validateIdentity(value); err != nil {
				return nil, err
			}
		}
	}
	return a, nil
}

// realmOf 把 mode/edition 字符串归一成地区标识：空 / global / intl → 国际版，cn → 国内版。
func realmOf(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case RealmCN, "china":
		return RealmCN
	default:
		return RealmGlobal
	}
}

func validateIdentity(value string) error {
	if len(value) > 256 || strings.Contains(value, "|") || hasControl(value) {
		return fmt.Errorf("Qoder 用户或设备标识无效")
	}
	return nil
}

func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// timestampMillis 把数字/数字字符串/RFC3339 归一成毫秒时间戳；0 与 null 视为「没有」。
func timestampMillis(raw json.RawMessage) *int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var num float64
	if err := json.Unmarshal(raw, &num); err == nil {
		return normalizeMillis(int64(num))
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if parsed, err := time.Parse(time.RFC3339, text); err == nil {
		ms := parsed.UnixMilli()
		return &ms
	}
	var asInt int64
	if _, err := fmt.Sscanf(text, "%d", &asInt); err == nil {
		return normalizeMillis(asInt)
	}
	return nil
}

func normalizeMillis(value int64) *int64 {
	if value <= 0 {
		return nil
	}
	if value < 100_000_000_000 { // 秒 → 毫秒
		value *= 1000
	}
	return &value
}

// SaveAtomic 以磁盘形态原子写回 FilePath（tmp + rename，0600）。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveAtomicLocked()
}

func (a *Auth) saveAtomicLocked() error {
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	var expires any
	if a.ExpiresAt > 0 {
		expires = a.ExpiresAt
	}
	doc := map[string]any{
		"mode":         a.Realm(),
		"accessToken":  a.AccessToken,
		"refreshToken": a.RefreshToken,
		"expiresAt":    expires,
		"userId":       a.UID,
		"email":        a.Email,
		"nickname":     a.Nickname,
		"machineId":    a.MachineID,
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	// 同一个文件可能被两个不同的 *Auth 对象写（面板登录新建的 vs 池里的），
	// 对象级锁保护不到；临时名带 pid + 序号，避免互相截断成半截 JSON。
	tmp := fmt.Sprintf("%s.tmp.%d.%d", a.FilePath, os.Getpid(), tmpSeq.Add(1))
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, a.FilePath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// LoadDir 扫描 dir 下 qoder-*.json。解析失败的文件跳过并留日志（静默跳过 = 账号凭空消失）。
// 不用 filepath.Glob：目录名带 [ ] * ? 时 Glob 会当模式解析。
func LoadDir(dir string) ([]*Auth, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "qoder-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		f := filepath.Join(dir, name)
		raw, err := os.ReadFile(f)
		if err != nil {
			log.Printf("[auth] 读不到账号文件 %s，跳过: %v", f, err)
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			log.Printf("[auth] 账号文件 %s 解析失败，跳过: %v", f, err)
			continue
		}
		if a.UID == "" {
			// 池按 uid 建索引：空 uid 的两个文件会互相覆盖，只剩一个号。
			a.UID = strings.TrimSuffix(strings.TrimPrefix(name, "qoder-"), ".json")
			log.Printf("[auth] %s 里没有 uid，按文件名补成 %q", f, a.UID)
		}
		a.FilePath = f
		out = append(out, a)
	}
	return out, nil
}

// FileNameFor 是账号落盘文件名（按 uid 稳定，便于人工核对）。
func FileNameFor(uid string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, uid)
	return "qoder-" + safe + ".json"
}
