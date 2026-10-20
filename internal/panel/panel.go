// Package panel 是内嵌的管理面板：账号池、额度、签到、网页登录、最近请求。
//
// 只做运维动作，不含业务逻辑——选号与转发全在 internal/server 与 internal/pool。
package panel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/pool"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/reqlog"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/upstream"
)

// Config 是面板依赖。
type Config struct {
	Pool     *pool.Pool
	Upstream *upstream.Client
	AuthDir  string
	AdminKey string
	APIKey   string
	Log      *reqlog.Recorder
	Version  string
	Checkin  bool
	LoginApp string
}

// Panel 是面板路由。
type Panel struct {
	cfg Config
	mux *http.ServeMux

	mu      sync.Mutex
	pending map[string]time.Time // 登录 state -> 过期时刻
}

// New 构造面板。
func New(cfg Config) *Panel {
	if cfg.LoginApp == "" {
		cfg.LoginApp = "办公小浣熊客户端"
	}
	p := &Panel{cfg: cfg, mux: http.NewServeMux(), pending: map[string]time.Time{}}
	p.mux.HandleFunc("GET /panel/{$}", p.index)
	p.mux.HandleFunc("GET /panel/app.js", p.appScript)
	p.mux.HandleFunc("GET /panel/api/overview", p.withAuth(p.overview))
	p.mux.HandleFunc("GET /panel/api/logs", p.withAuth(p.logsHandler))
	p.mux.HandleFunc("POST /panel/api/accounts/import", p.withAuth(p.accountImport))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/remove", p.withAuth(p.accountRemove))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/disable", p.withAuth(p.accountDisable))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/enable", p.withAuth(p.accountEnable))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/refresh", p.withAuth(p.accountRefresh))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/balance", p.withAuth(p.accountBalance))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/checkin", p.withAuth(p.accountCheckin))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/priority", p.withAuth(p.accountPriority))
	p.mux.HandleFunc("POST /panel/api/checkin_all", p.withAuth(p.checkinAll))
	p.mux.HandleFunc("POST /panel/api/balance_all", p.withAuth(p.balanceAll))
	p.mux.HandleFunc("POST /panel/api/login/start", p.withAuth(p.loginStart))
	p.mux.HandleFunc("POST /panel/api/login/exchange", p.withAuth(p.loginExchange))
	return p
}

// ServeHTTP 实现 http.Handler。
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) { p.mux.ServeHTTP(w, r) }

func (p *Panel) key() string {
	if p.cfg.AdminKey != "" {
		return p.cfg.AdminKey
	}
	return p.cfg.APIKey
}

func (p *Panel) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want := p.key()
		if want != "" {
			got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			if got == "" {
				got = r.URL.Query().Get("key")
			}
			if got != want {
				writeErr(w, http.StatusUnauthorized, "面板密钥无效")
				return
			}
		}
		next(w, r)
	}
}

// ---------------------------------------------------------------------------
// 静态资源

func (p *Panel) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(indexHTML))
}

func (p *Panel) appScript(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(appJS))
}

// ---------------------------------------------------------------------------
// 查询

func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	stats := reqlog.Stats{}
	if p.cfg.Log != nil {
		stats = p.cfg.Log.Stats()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     p.cfg.Version,
		"accounts":    p.cfg.Pool.Statuses(),
		"stats":       stats,
		"models":      p.catalog(r.Context()),
		"checkin":     p.cfg.Checkin,
		"recent":      p.recent(20),
		"server_time": time.Now().Format(time.RFC3339),
	})
}

// catalog 拉一次上游目录（失败给空列表，不阻塞面板加载）。
func (p *Panel) catalog(parent context.Context) []upstream.Model {
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	list, err := p.cfg.Upstream.Catalog(ctx, p.firstToken())
	if err != nil {
		return []upstream.Model{}
	}
	return list
}

func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": p.recent(200)})
}

func (p *Panel) recent(n int) []reqlog.Event {
	if p.cfg.Log == nil {
		return []reqlog.Event{}
	}
	return p.cfg.Log.List(n)
}

func (p *Panel) firstToken() string {
	for _, a := range p.cfg.Pool.Accounts() {
		if a.Enabled && a.Token() != "" {
			return a.Token()
		}
	}
	if accts := p.cfg.Pool.Accounts(); len(accts) > 0 {
		return accts[0].Token()
	}
	return ""
}

// ---------------------------------------------------------------------------
// 账号动作

type importReq struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Name         string `json:"name"`
	JSON         string `json:"json"` // 整份账号 JSON（面板粘贴框）
}

func (p *Panel) accountImport(w http.ResponseWriter, r *http.Request) {
	var req importReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "解析请求失败: "+err.Error())
		return
	}
	var acct *auth.Auth
	var err error
	switch {
	case strings.TrimSpace(req.JSON) != "":
		acct, err = auth.Parse([]byte(req.JSON))
	case strings.TrimSpace(req.AccessToken) != "":
		raw, _ := json.Marshal(map[string]string{
			"access_token":  req.AccessToken,
			"refresh_token": req.RefreshToken,
			"name":          req.Name,
		})
		acct, err = auth.Parse(raw)
	default:
		err = fmt.Errorf("请粘贴账号 JSON 或填写 access_token")
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if acct.Name == "" || acct.Name == acct.UID {
		if req.Name != "" {
			acct.Name = req.Name
		}
	}
	if err := acct.SaveAtomic(p.cfg.AuthDir); err != nil {
		writeErr(w, http.StatusInternalServerError, "写入账号文件失败: "+err.Error())
		return
	}
	p.cfg.Pool.Add(acct)
	// 顺手验一次：目录能拉到就说明 token 可用；拉不到只给提示，不拒收。
	warning := ""
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if _, err := p.cfg.Upstream.Catalog(ctx, acct.Token()); err != nil {
		warning = "账号已保存，但上游校验失败: " + err.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": acct.UID, "warning": warning})
}

func (p *Panel) accountRemove(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	acct := p.cfg.Pool.Remove(uid)
	if acct == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	if path := acct.Path(); path != "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			writeErr(w, http.StatusInternalServerError, "删除账号文件失败: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) accountDisable(w http.ResponseWriter, r *http.Request) {
	p.cfg.Pool.Disable(r.PathValue("uid"), "手动停用")
	p.save(w, r.PathValue("uid"))
}

func (p *Panel) accountEnable(w http.ResponseWriter, r *http.Request) {
	p.cfg.Pool.Enable(r.PathValue("uid"))
	p.save(w, r.PathValue("uid"))
}

func (p *Panel) accountPriority(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Priority int `json:"priority"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "解析请求失败: "+err.Error())
		return
	}
	uid := r.PathValue("uid")
	acct := p.cfg.Pool.Get(uid)
	if acct == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	acct.Priority = req.Priority
	p.save(w, uid)
}

func (p *Panel) accountRefresh(w http.ResponseWriter, r *http.Request) {
	acct := p.cfg.Pool.Get(r.PathValue("uid"))
	if acct == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	if acct.RefreshValue() == "" {
		writeErr(w, http.StatusBadRequest, "该账号没有 refresh_token，无法续期")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	access, refresh, err := p.cfg.Upstream.Refresh(ctx, acct.RefreshValue())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "续期失败: "+err.Error())
		return
	}
	acct.ApplyTokens(access, refresh)
	if err := acct.SaveAtomic(p.cfg.AuthDir); err != nil {
		writeErr(w, http.StatusInternalServerError, "写入账号文件失败: "+err.Error())
		return
	}
	p.cfg.Pool.ClearCooling(acct.UID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) accountBalance(w http.ResponseWriter, r *http.Request) {
	acct := p.cfg.Pool.Get(r.PathValue("uid"))
	if acct == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	bal, err := p.refreshBalance(r.Context(), acct)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "查询额度失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, bal)
}

func (p *Panel) accountCheckin(w http.ResponseWriter, r *http.Request) {
	acct := p.cfg.Pool.Get(r.PathValue("uid"))
	if acct == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	if err := p.cfg.Upstream.ClaimDailyGrant(ctx, acct.Token()); err != nil {
		writeErr(w, http.StatusBadGateway, "签到失败: "+err.Error())
		return
	}
	acct.MarkCheckedIn()
	_ = acct.SaveAtomic(p.cfg.AuthDir)
	bal, _ := p.refreshBalance(ctx, acct)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "balance": bal})
}

func (p *Panel) checkinAll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	results := map[string]string{}
	for _, acct := range p.cfg.Pool.Accounts() {
		if !acct.Enabled {
			continue
		}
		if err := p.cfg.Upstream.ClaimDailyGrant(ctx, acct.Token()); err != nil {
			results[acct.UID] = "失败: " + err.Error()
			continue
		}
		acct.MarkCheckedIn()
		_ = acct.SaveAtomic(p.cfg.AuthDir)
		results[acct.UID] = "ok"
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (p *Panel) balanceAll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	results := map[string]string{}
	for _, acct := range p.cfg.Pool.Accounts() {
		if _, err := p.refreshBalance(ctx, acct); err != nil {
			results[acct.UID] = "失败: " + err.Error()
			continue
		}
		results[acct.UID] = "ok"
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (p *Panel) refreshBalance(ctx context.Context, acct *auth.Auth) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	bal, err := p.cfg.Upstream.Balance(ctx, acct.Token())
	if err != nil {
		return nil, err
	}
	acct.SetCredit(bal.Available)
	_ = acct.SaveAtomic(p.cfg.AuthDir)
	return map[string]any{
		"available": bal.Available, "daily": bal.Daily, "monthly": bal.Monthly,
		"reward": bal.Reward, "topup": bal.Topup,
	}, nil
}

// ---------------------------------------------------------------------------
// 网页登录（授权页 → office-raccoon://auth/callback?code=… → 换票）

const loginTTL = 10 * time.Minute

func (p *Panel) loginStart(w http.ResponseWriter, _ *http.Request) {
	state, err := newState()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成 state 失败: "+err.Error())
		return
	}
	expires := time.Now().Add(loginTTL)
	p.mu.Lock()
	for s, at := range p.pending {
		if time.Now().After(at) {
			delete(p.pending, s)
		}
	}
	p.pending[state] = expires
	p.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"url":        p.cfg.Upstream.AuthorizeURL(state, p.cfg.LoginApp),
		"state":      state,
		"expires_at": expires.Format(time.RFC3339),
	})
}

func (p *Panel) loginExchange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code  string `json:"code"`
		State string `json:"state"`
		URL   string `json:"url"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "解析请求失败: "+err.Error())
		return
	}
	code, state := strings.TrimSpace(req.Code), strings.TrimSpace(req.State)
	if strings.TrimSpace(req.URL) != "" {
		c, s, ok := upstream.ParseCallbackCode(req.URL)
		if !ok {
			writeErr(w, http.StatusBadRequest, "回调链接不合法，应形如 office-raccoon://auth/callback?code=…&state=…")
			return
		}
		code, state = c, s
	}
	if code == "" || state == "" {
		writeErr(w, http.StatusBadRequest, "缺少 code 或 state")
		return
	}
	p.mu.Lock()
	expires, ok := p.pending[state]
	if ok {
		delete(p.pending, state)
	}
	p.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusBadRequest, "登录 state 不存在或已使用，请重新发起登录")
		return
	}
	if time.Now().After(expires) {
		writeErr(w, http.StatusBadRequest, "登录 state 已过期，请重新发起登录")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := p.cfg.Upstream.ExchangeLoginCode(ctx, code)
	if err != nil {
		status := http.StatusBadGateway
		var ue *upstream.Error
		if errors.As(err, &ue) && ue.Kind == upstream.KindClient {
			status = http.StatusBadRequest
		}
		writeErr(w, status, "换取凭证失败: "+err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = res.OrgName
	}
	if name == "" {
		name = res.Identity
	}
	acct := auth.New(res.AccessToken, res.RefreshToken, name)
	if err := acct.SaveAtomic(p.cfg.AuthDir); err != nil {
		writeErr(w, http.StatusInternalServerError, "写入账号文件失败: "+err.Error())
		return
	}
	p.cfg.Pool.Add(acct)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": acct.UID, "name": acct.Name})
}

// ---------------------------------------------------------------------------
// 工具

func (p *Panel) save(w http.ResponseWriter, uid string) {
	acct := p.cfg.Pool.Get(uid)
	if acct == nil {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	if err := acct.SaveAtomic(p.cfg.AuthDir); err != nil {
		writeErr(w, http.StatusInternalServerError, "写入账号文件失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func newState() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
