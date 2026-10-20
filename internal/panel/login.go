package panel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/upstream"
)

const (
	loginTTL = 15 * time.Minute
)

var errLoginSession = errors.New("登录链接已失效，请回面板重新点「添加账号」")

// loginSession 是一次设备授权流程（一个账号一个全新设备号）。
type loginSession struct {
	Machine string
	Realm   string
	Login   *upstream.DeviceLogin
	At      time.Time
	// Busy 上一次 poll 还没回来：前端 3s 一次比上游慢时会重叠，重叠会并发打同一个 nonce。
	Busy bool
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// newMachineID 生成账号的设备指纹（UUID4，与官方客户端同形）。
func newMachineID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return strings.Join([]string{
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	}, "-")
}

// authCallback 接官方授权页的回调（无需 API key：这是浏览器跳转，不是面板前端调用）。
//
// 只做两件事：核对 state 属于哪个未完成的登录会话、把 code 交给它。换票在 login/poll 里做，
// 这样浏览器那边一提交就能立刻回一个干净的成功页，网络慢也不拖住页面。
//
// 安全边界：state 是 32 字节随机数，只存在内存里、5~15 分钟内一次性有效；code 本身也是
// 一次性授权码（上游 300 秒过期）。就算有人扫到这个端点，没有 state 也塞不进账号。
func (p *Panel) authCallback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	state, code := query.Get("state"), query.Get("code")
	// 官方页在用户取消/失败时也可能带 error 回来。
	if msg := query.Get("error"); msg != "" {
		writeCallbackPage(w, http.StatusBadRequest, "授权未完成", msg)
		return
	}
	if state == "" || code == "" {
		writeCallbackPage(w, http.StatusBadRequest, "回调参数不全", "缺少 state 或 code")
		return
	}
	p.loginMu.Lock()
	sess, ok := p.logins[state]
	p.loginMu.Unlock()
	if !ok || time.Since(sess.At) > loginTTL {
		writeCallbackPage(w, http.StatusGone, "登录会话已失效", "请回到面板重新点「添加账号」")
		return
	}
	if err := sess.Login.AcceptCallback(state, code); err != nil {
		writeCallbackPage(w, http.StatusBadRequest, "无法接收授权", err.Error())
		return
	}
	log.Printf("panel: 收到浏览器授权回调 state=%.8s…", state)
	writeCallbackPage(w, http.StatusOK, "授权成功", "可以关闭这个页面，回到面板即可（账号会自动出现）")
}

// writeCallbackPage 给浏览器看的极简结果页：不引外部资源、不回显 code。
func writeCallbackPage(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>`+html.EscapeString(title)+
		`</title><body style="font:15px/1.6 system-ui,Segoe UI,Microsoft YaHei,sans-serif;padding:56px;color:#222">`+
		`<h2 style="margin:0 0 8px">`+html.EscapeString(title)+`</h2><p style="color:#555;margin:0">`+
		html.EscapeString(detail)+`</p></body>`)
}

// loginStart 生成设备授权链接：{id, url}。用户在自己浏览器里打开 url 完成授权。
func (p *Panel) loginStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Realm string `json:"realm"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req)
	// 小浣熊目前只有国内版（RegionGlobal 也 == "cn"）。国际版分支先留着：
	// 等官方出国际站/国际客户端，把这里改回「按 req.Realm 分流」即可。
	_ = req.Realm
	realm := upstream.RegionCN
	machine := newMachineID()
	// 环回回调：浏览器登录完由官方授权页把 code 打到面板自己的 /auth/callback，
	// 全程不需要官方客户端、也不需要人工粘贴 code。
	login, err := upstream.NewDeviceLogin(p.cfg.Upstream, realm, machine, p.cfg.CallbackBase)
	if err != nil {
		writeErr(w, http.StatusNotImplemented, err.Error())
		return
	}
	p.loginMu.Lock()
	if p.logins == nil {
		p.logins = map[string]loginSession{}
	}
	// 顺手清掉过期会话，避免面板长开时 map 无限增长。
	for id, sess := range p.logins {
		if time.Since(sess.At) > loginTTL {
			delete(p.logins, id)
		}
	}
	p.logins[login.State()] = loginSession{Machine: machine, Realm: realm, Login: login, At: time.Now()}
	p.loginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":    login.State(),
		"url":   login.AuthURL(),
		"realm": realm,
	})
}

// loginPoll 查一次授权结果：未完成 → {done:false}（前端每 3s 再问）；
// 会话不存在/过期 → 410（前端停止轮询）；完成 → 落盘入池并回 {done:true}。
//
// 做成「一次一查」而不是服务端长等：设备流没有回调，verifier 又只能在服务端，
// 所以必须有人轮询——让浏览器按固定节拍问，用户什么时候授权完都在下一次问到时收号，
// 不需要再点一次「我已授权」。
func (p *Panel) loginPoll(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	p.loginMu.Lock()
	sess, ok := p.logins[id]
	if ok && sess.Busy {
		// 上一次查询还没回来（上游慢）：本轮直接说「没完成」，避免并发打同一个 nonce。
		p.loginMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"done": false})
		return
	}
	if ok {
		sess.Busy = true
		p.logins[id] = sess
	}
	p.loginMu.Unlock()
	if !ok || time.Since(sess.At) > loginTTL {
		writeErr(w, http.StatusGone, errLoginSession.Error())
		return
	}
	// 失败路径复位闸门，供下一次轮询继续（成功路径已 delete 掉会话）。
	defer func() {
		p.loginMu.Lock()
		if s, still := p.logins[id]; still {
			s.Busy = false
			p.logins[id] = s
		}
		p.loginMu.Unlock()
	}()

	creds, done, err := sess.Login.Poll(r.Context())
	if err != nil {
		// 5xx/超时交给前端继续重试：授权可能已在浏览器侧完成，停掉就永远不入池。
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if !done {
		writeJSON(w, http.StatusOK, map[string]any{"done": false})
		return
	}
	uid, nick, err := p.saveAccount(creds)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.loginMu.Lock()
	delete(p.logins, id)
	p.loginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"done": true, "ok": true, "uid": uid, "nickname": nick})
}

// saveAccount 把一个已确认的凭证落盘并入池（同一 uid 直接覆盖旧文件）。
func (p *Panel) saveAccount(creds *auth.Auth) (uid, nickname string, err error) {
	if creds.UID == "" {
		return "", "", errors.New("上游没有返回用户标识，无法保存账号")
	}
	creds.FilePath = p.cfg.AuthDir + "/" + auth.FileNameFor(creds.UID)
	if err := creds.SaveAtomic(); err != nil {
		return "", "", err
	}
	p.cfg.Pool.Add(creds)
	log.Printf("panel: 账号已入池 uid=%s realm=%s", creds.UID, creds.Realm())
	return creds.UID, creds.Nickname, nil
}

// loginRefresh 用一条 refreshToken 直登（最稳的免粘贴入口）。
func (p *Panel) loginRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
		Realm        string `json:"realm"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	rt := strings.TrimSpace(req.RefreshToken)
	if rt == "" {
		writeErr(w, http.StatusBadRequest, "请填写 refreshToken")
		return
	}
	// 小浣熊的 refresh_token 是上游直给的 JWT，没有 Qoder 那种 token|uid|machine 打包串。
	creds := &auth.Auth{Data: auth.Data{RefreshToken: rt}}
	if err := p.completeByRefresh(r.Context(), creds); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if creds.UID == "" {
		writeErr(w, http.StatusBadGateway, "上游没有返回用户标识，无法保存账号")
		return
	}
	if err := creds.CompleteIdentity(newMachineID()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	uid, nick, err := p.saveAccount(creds)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "nickname": nick})
}

// completeByRefresh 续期一次并补齐 uid/昵称（refresh 响应可能不带 user_id，再查 userinfo）。
func (p *Panel) completeByRefresh(ctx context.Context, creds *auth.Auth) error {
	if err := p.cfg.Upstream.RefreshToken(creds); err != nil {
		return err
	}
	uid, nick, _, err := p.cfg.Upstream.GetUserInfo(creds)
	if err != nil {
		if creds.UID == "" {
			return err
		}
		return nil
	}
	if uid != "" {
		if creds.UID != "" && creds.UID != uid {
			return errors.New("续期返回了不同账号，旧凭证未被覆盖")
		}
		creds.UID = uid
	}
	if nick != "" {
		creds.Nickname = nick
	}
	return nil
}
