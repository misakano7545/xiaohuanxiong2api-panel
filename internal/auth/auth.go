// Package auth 解析小浣熊账号凭证：磁盘形态、原子写回与目录扫描。
//
// 一个账号一个 JSON（auths/<uid>.json，0600）：
//
//	{"access_token":"eyJ…","refresh_token":"eyJ…","expires_at":1792369369000,
//	 "uid":"…","nickname":"小浣熊-1","credit":1234,"checked_in":"2026-10-20"}
//
// access/refresh 都是上游直给的 JWT；上行每次续期都会轮换 refresh_token，
// 所以新票**必须**落盘才算真到手（见 SaveAtomic）。
package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// tmpSeq 让同一个账号文件的临时名在进程内唯一（见 SaveAtomic）。
var tmpSeq atomic.Uint64

// 地区常量保留（面板/日志/模型表沿用），小浣熊只有国内一个站点。
const (
	RealmCN     = "cn"
	RealmGlobal = "cn"
	RealmIntl   = "cn"
)

// MaxTokenLength 是 token 字段长度上限（与上游客户端同一口径）。
const MaxTokenLength = 8192

// Auth 归一化后的账号凭证。
//
// 并发模型：mu 保护可变字段（AccessToken/RefreshToken/ExpiresAt/Credit/CheckedIn），
// 写路径（upstream 续期）持写锁整段执行，读路径持读锁取快照。
type Auth struct {
	Data
	mu       sync.RWMutex
	FilePath string `json:"-"`
}

// Data 是可序列化字段（不含锁与路径，避免拷锁）。
type Data struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"` // Unix 毫秒；0 = 未知
	UID          string `json:"uid"`
	Nickname     string `json:"nickname,omitempty"`
	Email        string `json:"email,omitempty"`
	// MachineID 是小浣熊没有的东西（设备指纹是 Qoder 的），保留字段只为让基线池代码
	// 的「换指纹」路径有个稳定落点，恒空。
	MachineID string `json:"machine_id,omitempty"`

	// 额度与签到是最近一次查询的快照（面板展示用），随账号文件一起持久化。
	Credit    int64  `json:"credit"`
	CreditAt  string `json:"credit_at,omitempty"`
	CheckedIn string `json:"checked_in,omitempty"`
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

// SetMachineID 写入设备指纹（小浣熊不用，保留基线接口）。
func (a *Auth) SetMachineID(id string) {
	a.mu.Lock()
	a.MachineID = id
	a.mu.Unlock()
}

// MachineIDValue 读设备指纹。
func (a *Auth) MachineIDValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.MachineID
}

// Realm 返回地区标识；小浣熊只有国内站，恒 "cn"。
func (a *Auth) Realm() string { return RealmCN }

// CanRefresh 报告是否有刷新凭证。
func (a *Auth) CanRefresh() bool { return a.RefreshTokenValue() != "" }

// NeedsRefresh 报告 token 是否将在 within 内过期（无 expiry = 不主动刷，等 401 再刷）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.NeedsRefreshLocked(within)
}

// NeedsRefreshLocked 是 NeedsRefresh 的持锁内部版本。
func (a *Auth) NeedsRefreshLocked(within time.Duration) bool {
	if a.ExpiresAt <= 0 || a.RefreshToken == "" {
		return false
	}
	return time.Now().Add(within).UnixMilli() >= a.ExpiresAt
}

// SetTokens 写入一组新凭证（调用方随后负责 SaveAtomic）。
func (a *Auth) SetTokens(access, refresh string, expiresAtMs int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if access != "" {
		a.AccessToken = stripBearer(access)
		a.ExpiresAt = expiresAtMs
		if a.ExpiresAt == 0 {
			a.ExpiresAt = JWTExpiry(a.AccessToken)
		}
	}
	if refresh != "" {
		a.RefreshToken = stripBearer(refresh)
	}
}

// SetCredit 记录额度快照。
func (a *Auth) SetCredit(credit int64) {
	a.mu.Lock()
	a.Credit = credit
	a.CreditAt = time.Now().Format(time.RFC3339)
	a.mu.Unlock()
}

// MarkCheckedIn 记一次签到成功。
func (a *Auth) MarkCheckedIn() {
	a.mu.Lock()
	a.CheckedIn = time.Now().Format("2006-01-02")
	a.mu.Unlock()
}

// CheckedInToday 报告今天是否已签到。
func (a *Auth) CheckedInToday() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.CheckedIn == time.Now().Format("2006-01-02")
}

// FileNameFor 给出账号文件的规范名。
func FileNameFor(uid string) string {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return "unknown.json"
	}
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, uid)
	return safe + ".json"
}

// SaveAtomic 原子写回（临时文件 + rename，0600）。FilePath 为空时退到 FileNameFor，
// 调用方负责给绝对路径；写失败必须让调用方看见——轮换过的 refresh_token 丢了就再也刷不了。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	path := a.FilePath
	if path == "" && a.UID != "" {
		path = FileNameFor(a.UID)
	}
	snap := a.snapshotLocked()
	a.mu.Unlock()
	if path == "" {
		return errors.New("账号没有落盘路径")
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := fmt.Sprintf("%s.%d-%d.tmp", path, os.Getpid(), tmpSeq.Add(1))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// snapshotLocked 返回可序列化副本（调用方持锁）。
func (a *Auth) snapshotLocked() Data {
	return Data{
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		ExpiresAt:    a.ExpiresAt,
		UID:          a.UID,
		Nickname:     a.Nickname,
		Email:        a.Email,
		MachineID:    a.MachineID,
		Credit:       a.Credit,
		CreditAt:     a.CreditAt,
		CheckedIn:    a.CheckedIn,
	}
}

// Snapshot 返回读锁副本。
func (a *Auth) Snapshot() Data {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.snapshotLocked()
}

// Parse 解析一份账号 JSON（面板粘贴 / 磁盘文件 / 上游响应同形）。
// uid 优先取显式字段，其次从 access token 的 JWT 里解；都没有就用 token 哈希兜底。
func Parse(raw []byte) (*Auth, error) {
	var in struct {
		AccessToken  string `json:"access_token"`
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    any    `json:"expires_at"`
		UID          string `json:"uid"`
		UserID       string `json:"user_id"`
		Nickname     string `json:"nickname"`
		Name         string `json:"name"`
		Email        string `json:"email"`
		Credit       int64  `json:"credit"`
		CheckedIn    string `json:"checked_in"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("解析账号 JSON: %w", err)
	}
	access := stripBearer(firstNonEmpty(in.AccessToken, in.Token))
	if access == "" {
		return nil, errors.New("缺少 access_token")
	}
	if len(access) > MaxTokenLength {
		return nil, errors.New("access_token 过长")
	}
	a := &Auth{Data: Data{
		AccessToken:  access,
		RefreshToken: stripBearer(in.RefreshToken),
		ExpiresAt:    millisOf(in.ExpiresAt),
		UID:          firstNonEmpty(in.UID, in.UserID),
		Nickname:     firstNonEmpty(in.Nickname, in.Name),
		Email:        in.Email,
		Credit:       in.Credit,
		CheckedIn:    in.CheckedIn,
	}}
	if a.ExpiresAt == 0 {
		a.ExpiresAt = JWTExpiry(access)
	}
	if a.UID == "" {
		a.UID = JWTUserID(access)
	}
	if a.UID == "" {
		a.UID = "t" + hashTail(firstNonEmpty(a.RefreshToken, access))
	}
	if a.Nickname == "" {
		a.Nickname = a.UID
	}
	return a, nil
}

// LoadDir 读目录下所有 *.json 账号（单个坏文件只跳过，不阻塞启动）。
func LoadDir(dir string) ([]*Auth, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]*Auth, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = path
		out = append(out, a)
	}
	return out, nil
}

// CompleteIdentity 用 JWT 补齐 uid/昵称（解析时可能缺，续期后就到齐了）。
func (a *Auth) CompleteIdentity(_ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.UID == "" {
		a.UID = JWTUserID(a.AccessToken)
	}
	if a.UID == "" {
		return errors.New("无法从 token 解析用户标识，请粘贴含 uid 的完整账号记录")
	}
	if a.Nickname == "" {
		a.Nickname = a.UID
	}
	return nil
}

func stripBearer(v string) string {
	v = strings.TrimSpace(v)
	return strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func hashTail(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])[:10]
}

// JWTExpiry 读 JWT payload 的 exp（毫秒）；解不出返回 0。不验签。
func JWTExpiry(token string) int64 {
	claims := jwtClaims(token)
	if claims == nil {
		return 0
	}
	raw, ok := claims["exp"]
	if !ok {
		return 0
	}
	var sec float64
	if json.Unmarshal(raw, &sec) != nil || sec <= 0 {
		return 0
	}
	return int64(sec * 1000)
}

// JWTUserID 读 JWT payload 里的用户 id；解不出返回空串。
func JWTUserID(token string) string {
	claims := jwtClaims(token)
	if claims == nil {
		return ""
	}
	for _, key := range []string{"user_id", "userId", "uid", "userID", "sub"} {
		raw, ok := claims[key]
		if !ok {
			continue
		}
		var text string
		if json.Unmarshal(raw, &text) == nil && text != "" {
			return text
		}
		var num json.Number
		if json.Unmarshal(raw, &num) == nil && num.String() != "" && num.String() != "0" {
			return num.String()
		}
	}
	return ""
}

// jwtClaims 解出 JWT payload 的字段表（base64url，不验签）。
func jwtClaims(token string) map[string]json.RawMessage {
	parts := strings.Split(stripBearer(token), ".")
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if payload, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return nil
		}
	}
	var claims map[string]json.RawMessage
	if json.Unmarshal(payload, &claims) != nil {
		return nil
	}
	return claims
}

// millisOf 把数字/数字字符串/RFC3339 归一成毫秒时间戳（0 = 没有）。
func millisOf(value any) int64 {
	switch v := value.(type) {
	case float64:
		return normalizeMillis(int64(v))
	case int64:
		return normalizeMillis(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return normalizeMillis(n)
		}
	case string:
		trimmed := strings.TrimSpace(v)
		if n, err := json.Number(trimmed).Int64(); err == nil {
			return normalizeMillis(n)
		}
		if t, err := time.Parse(time.RFC3339, trimmed); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

func normalizeMillis(v int64) int64 {
	if v <= 0 {
		return 0
	}
	if v < 100_000_000_000 {
		return v * 1000
	}
	return v
}

// MaskToken 只回末 4 位（面板展示用）。
func MaskToken(token string) string {
	r := []rune(stripBearer(token))
	if len(r) <= 8 {
		return "«" + fmt.Sprint(len(r)) + " 字符»"
	}
	return "…" + string(r[len(r)-4:])
}
