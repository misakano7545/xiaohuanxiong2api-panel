// Package auth 是单个小浣熊账号的凭证（access_token / refresh_token）与其落盘。
//
// 一个账号一个文件（auths/<uid>.json），原子写（临时文件 + rename，0600）——
// 上游的 refresh_token 每次刷新都会轮换，新值只有落盘成功才算真到手。
package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Data 是一个账号的可序列化字段（不含锁与路径，避免拷锁）。
type Data struct {
	UID          string `json:"uid"`
	Name         string `json:"name"`
	UserID       string `json:"user_id,omitempty"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"` // 毫秒；0 = 未知
	Enabled      bool   `json:"enabled"`
	Priority     int    `json:"priority,omitempty"`
	Note         string `json:"note,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`

	// Credit 是最近一次查询到的可用积分（仅展示，-1 = 未知）。
	Credit    int64  `json:"credit,omitempty"`
	CreditAt  string `json:"credit_at,omitempty"`
	CheckedIn string `json:"checked_in,omitempty"` // 最近签到成功的日期 YYYY-MM-DD
}

// Auth 是一个账号：Data 加一把锁和落盘路径。
type Auth struct {
	Data
	path string
	mu   sync.RWMutex
}

// New 用一对 token 建账号：uid 取 JWT 里的用户 id，取不到就用 token 的哈希。
func New(access, refresh, name string) *Auth {
	access = stripBearer(access)
	refresh = stripBearer(refresh)
	uid := JWTUserID(access)
	if uid == "" {
		uid = JWTUserID(refresh)
	}
	if uid == "" {
		seed := refresh
		if seed == "" {
			seed = access
		}
		sum := sha256.Sum256([]byte(seed))
		uid = "t" + hex.EncodeToString(sum[:])[:10]
	}
	if name = strings.TrimSpace(name); name == "" {
		name = uid
	}
	return &Auth{Data: Data{
		UID:          uid,
		Name:         name,
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresAt:    JWTExpiry(access),
		Enabled:      true,
		CreatedAt:    time.Now().Format(time.RFC3339),
		Credit:       -1,
	}}
}

// Parse 解析面板/手工粘贴进来的一份账号 JSON。
func Parse(raw []byte) (*Auth, error) {
	var in struct {
		Data
		Enabled *bool `json:"enabled"` // 缺省 true：没写这个键时不能让新导入的号立刻不可用
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("解析账号 JSON: %w", err)
	}
	access := stripBearer(in.AccessToken)
	if access == "" {
		return nil, fmt.Errorf("缺少 access_token")
	}
	base := New(access, in.RefreshToken, in.Name)
	if in.UID != "" {
		base.UID = in.UID
	}
	if in.UserID != "" {
		base.UserID = in.UserID
	}
	if in.ExpiresAt > 0 {
		base.ExpiresAt = in.ExpiresAt
	}
	base.Priority = in.Priority
	base.Note = in.Note
	if in.CreatedAt != "" {
		base.CreatedAt = in.CreatedAt
	}
	if in.Enabled != nil {
		base.Enabled = *in.Enabled
	}
	// 额度/签到是账号文件里的运行期快照，重启后要能继续展示，不能被 New() 的默认值盖掉。
	base.Credit, base.CreditAt, base.CheckedIn = in.Credit, in.CreditAt, in.CheckedIn
	return base, nil
}

// LoadDir 读取目录下所有 *.json 账号（单个损坏文件只跳过，不阻塞启动）。
func LoadDir(dir string) ([]*Auth, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Auth
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
		a.path = path
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}

// Path 返回落盘路径（未设置时为空）。
func (a *Auth) Path() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.path
}

// SetPath 设置落盘路径。
func (a *Auth) SetPath(p string) {
	a.mu.Lock()
	a.path = p
	a.mu.Unlock()
}

// Token 返回当前 access token。
func (a *Auth) Token() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken
}

// RefreshValue 返回当前 refresh token。
func (a *Auth) RefreshValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Data.RefreshToken
}

// Expiry 返回 access token 过期时刻（零值 = 未知）。
func (a *Auth) Expiry() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.ExpiresAt <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(a.ExpiresAt)
}

// NeedsRefresh 报告是否该续期。过期时间未知时不主动刷（上游会在 401 时触发）。
func (a *Auth) NeedsRefresh(skew time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.RefreshToken == "" || a.ExpiresAt <= 0 {
		return false
	}
	return time.Now().Add(skew).UnixMilli() >= a.ExpiresAt
}

// ApplyTokens 落新票（refresh 为空表示上游未轮换，沿用旧值）。
func (a *Auth) ApplyTokens(access, refresh string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if access != "" {
		a.AccessToken = stripBearer(access)
		a.ExpiresAt = JWTExpiry(a.AccessToken)
		if a.UserID == "" {
			a.UserID = JWTUserID(a.AccessToken)
		}
	}
	if refresh != "" {
		a.RefreshToken = stripBearer(refresh)
	}
}

// SetCredit 记录额度快照。
func (a *Auth) SetCredit(c int64) {
	a.mu.Lock()
	a.Credit = c
	a.CreditAt = time.Now().Format(time.RFC3339)
	a.mu.Unlock()
}

// MarkCheckedIn 记一次签到成功。
func (a *Auth) MarkCheckedIn() {
	a.mu.Lock()
	a.CheckedIn = time.Now().Format("2006-01-02")
	a.mu.Unlock()
}

// Snapshot 返回可序列化副本。
func (a *Auth) Snapshot() Data {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Data
}

// SaveAtomic 原子落盘。路径未设置时写到 dir/uid.json。
func (a *Auth) SaveAtomic(dir string) error {
	a.mu.Lock()
	path := a.path
	if path == "" && dir != "" {
		path = filepath.Join(dir, a.UID+".json")
		a.path = path
	}
	snap := a.Data
	a.mu.Unlock()
	if path == "" {
		return fmt.Errorf("账号 %s 没有落盘路径", snap.UID)
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".acct-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// stripBearer 去掉 "Bearer " 前缀（粘贴来自抓包，常带前缀）。
func stripBearer(v string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "Bearer "))
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
	text := strings.Trim(string(raw), `"`)
	sec, err := strconv.ParseFloat(text, 64)
	if err != nil || sec <= 0 {
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
	token = stripBearer(token)
	parts := strings.Split(token, ".")
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

// TokenTail 返回 token 末 4 位。
func TokenTail(token string) string {
	r := []rune(stripBearer(token))
	if len(r) <= 6 {
		return "…"
	}
	return "…" + string(r[len(r)-4:])
}
