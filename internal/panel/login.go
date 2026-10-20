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
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/upstream"
)

const (
	loginTTL = 15 * time.Minute
)

var errLoginSession = errors.New("登录链接已失效，请回面板重新点「添加账号」")

// loginSession 是一次登录流程（一个账号一个全新设备号）。
//
// 两种形态：Qr != nil 时走微信扫码轮询（不需要任何回调地址，服务器/局域网部署都能用）；
// 否则走 Qr == nil 的环回回调（浏览器登录后打 /auth/callback）。
type loginSession struct {
	Machine string
	Realm   string
	Login   *upstream.DeviceLogin
	Qr      *upstream.QrLogin
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

// callbackPort 从环回回调基址里取端口（只用于前端提示文案）。
func callbackPort(base string) string {
	if u, err := url.Parse(base); err == nil && u.Port() != "" {
		return u.Port()
	}
	return ""
}

// isLoopbackRequest 面板页面是否从本机回环地址打开的（决定环回回调能否自动完成）。
func isLoopbackRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || strings.EqualFold(host, "localhost") || host == "::1"
}

// loginCode 收人工粘贴的回调链接：面板跑在服务器/局域网时，官方页那条 127.0.0.1 回调
// 浏览器到不了（地址是写死的），用户把地址栏整条链接（或 office-raccoon:// 链接、裸 code）
// 粘进来即可，效果与环回回调完全一样。
//
// 两条路径：state 命中未完成会话 → 交回该会话（前端现有轮询继续跑，UI 行为一致）；
// 命中不了（换机/会话过期/只有 code）→ 直接换票入池。
func (p *Panel) loginCode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	code, state, ok := upstream.ParseCallbackCode(req.URL)
	if !ok {
		writeErr(w, http.StatusBadRequest,
			"认不出这条链接：请粘 http://127.0.0.1:<端口>/auth/callback?code=…&state=…、"+
				"office-raccoon://auth/callback?code=… 或 ac_ 开头的授权码")
		return
	}
	if state != "" {
		p.loginMu.Lock()
		sess, found := p.logins[state]
		p.loginMu.Unlock()
		if found && time.Since(sess.At) <= loginTTL {
			if err := sess.Login.AcceptCallback(state, code); err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			log.Printf("panel: 收到粘贴的授权回调 state=%.8s…", state)
			// 交回会话后由前端轮询换票，这里只报告已接收。
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "session", "id": state})
			return
		}
	}
	// 没有对应会话：自己换票（PrepareAccount 与「导入凭证」同一条管线）。
	creds, err := p.cfg.Upstream.PrepareAccount(r.Context(), map[string]any{"code": code}, newMachineID())
	if err != nil {
		writeErr(w, http.StatusBadGateway, publicErr(err))
		return
	}
	uid, nick, err := p.saveAccount(creds)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "imported", "uid": uid, "nickname": nick})
}

// loginQrStart 开一次微信扫码登录：返回会话 id、二维码内容、二维码 PNG 地址。
//
// 比环回回调强的地方：二维码里编的是**公开网页**（{origin}/login/mp?code=…），
// 微信扫完服务端把结果挂到我们自造的 code 上，面板轮询取票。所以面板放在服务器/
// 局域网、甚至用户用手机扫码都能完成，不再受「官方页只认 127.0.0.1」的限制。
func (p *Panel) loginQrStart(w http.ResponseWriter, r *http.Request) {
	machine := newMachineID()
	qr, err := upstream.NewQrLogin(p.cfg.Upstream, machine)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	p.loginMu.Lock()
	if p.logins == nil {
		p.logins = map[string]loginSession{}
	}
	for id, sess := range p.logins {
		if time.Since(sess.At) > loginTTL {
			delete(p.logins, id)
		}
	}
	p.logins[qr.State()] = loginSession{Machine: machine, Realm: upstream.RegionCN, Qr: qr, At: time.Now()}
	p.loginMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      qr.State(),
		"code":    qr.QRCode(),
		"content": qr.QRContent(),
		"png":     "/panel/api/login/qr.png?code=" + url.QueryEscape(qr.QRCode()),
	})
}

// loginQrImage 把扫码链接渲染成 PNG（二维码由面板自己画，不依赖任何外部服务）。
//
// 为什么服务端画而不是前端画：前端要么引 CDN 库（面板要能离线/内网用），要么
// 塞一份 QR 编码器进 app.js（几百行纠错码逻辑，维护成本高于收益）。
func (p *Panel) loginQrImage(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" || len(code) > 64 {
		writeErr(w, http.StatusBadRequest, "缺少 code")
		return
	}
	content := p.cfg.Upstream.QrContent(code)
	png, err := qrcode.Encode(content, qrcode.Medium, 320)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "二维码生成失败："+err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store") // code 一次性，缓存住会展示过期码
	_, _ = w.Write(png)
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
		// manual：当前页面不是从本机 127.0.0.1 打开的（局域网 IP / 域名 / 反代），
		// 官方页那条 127.0.0.1 回调浏览器到不了，前端应引导用户粘贴回调链接。
		"manual":        !isLoopbackRequest(r),
		"callback_port": callbackPort(p.cfg.CallbackBase),
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

	var creds *auth.Auth
	var done bool
	var err error
	if sess.Qr != nil {
		creds, done, err = sess.Qr.Poll(r.Context())
		if errors.Is(err, upstream.ErrQrCanceled) {
			// 用户取消/二维码过期：这是终态，停掉前端轮询比一直转圈诚实。
			p.loginMu.Lock()
			delete(p.logins, id)
			p.loginMu.Unlock()
			writeErr(w, http.StatusGone, err.Error())
			return
		}
	} else {
		creds, done, err = sess.Login.Poll(r.Context())
	}
	if err != nil {
		// 5xx/超时交给前端继续重试：授权可能已在浏览器侧完成，停掉就永远不入池。
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if !done {
		out := map[string]any{"done": false}
		if sess.Qr != nil {
			// 扫码没有回调事件：把上游状态（等待扫码 / 已扫码待确认）当进度显示。
			out["qr_status"] = sess.Qr.LastStatus()
		}
		writeJSON(w, http.StatusOK, out)
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
