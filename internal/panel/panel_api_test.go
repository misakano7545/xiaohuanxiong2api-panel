package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/pool"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/reqlog"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/upstream"
)

// newTestPanel 组装一个带单账号、指向假上游的面板。
func newTestPanel(t *testing.T) (*Panel, *upstream.Client, *pool.Pool) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/model_catalog":
			_, _ = io.WriteString(w, `{"code":0,"data":{"categories":[{"type":"chat","models":[
				{"name":"raccoon-chat-ml-5-5","display_name":"Raccoon Chat","tags":["thinking"],
				 "context_window":"180000","max_tokens":80000,"billing_multiplier":1,"visible":true}]}]}}`)
		case "/balance":
			_, _ = io.WriteString(w, `{"code":0,"data":{"available_points":120,"daily_points":0,
				"monthly_points":50,"reward_points":10,"topup_points":30}}`)
		case "/bills":
			_, _ = io.WriteString(w, `{"code":0,"data":{"items":[]}}`)
		case "/api/web/desktop/v1/login/points/grant":
			_, _ = io.WriteString(w, `{"code":0,"data":{"granted":false,"popup":null}}`)
		case "/user_info":
			// 按令牌回身份：access-1 是 TestImportPAT 导入号的票，code-access 是授权码
			// 换来的票（office uid 以 user_info 为准，不是换票响应里的平台 id），别的按 u-1。
			switch r.Header.Get("Authorization") {
			case "Bearer access-1":
				_, _ = io.WriteString(w, `{"code":0,"data":{"id":"u-2","name":"导入号"}}`)
			case "Bearer code-access":
				_, _ = io.WriteString(w, `{"code":0,"data":{"id":"u-9","name":"授权号"}}`)
			default:
				_, _ = io.WriteString(w, `{"code":0,"data":{"id":"u-1","name":"测试号"}}`)
			}
		case "/refresh":
			_, _ = io.WriteString(w, `{"code":0,"data":{"access_token":"fresh","refresh_token":"fresh-r","user_id":"u-1"}}`)
		case "/login_with_authorization_code":
			_, _ = io.WriteString(w, `{"code":0,"data":{"access_token":"code-access","user_id":"u-9",
				"refresh_token":"code-refresh","office_org_name":"测试企业"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	up := upstream.New()
	up.SetAgentHosts(srv.URL, srv.URL)
	up.SetAPIHosts(srv.URL, srv.URL)

	p := pool.New("")
	p.Add(&auth.Auth{Data: auth.Data{
		AccessToken: "dt-1", RefreshToken: "drt-1",
		ExpiresAt: time.Now().Add(48 * time.Hour).UnixMilli(),
		MachineID: "m-1", UID: "u-1", Nickname: "测试号",
	}})
	h := &apiShim{up: up}
	pn := New(Config{
		Pool: p, Upstream: up, AuthDir: t.TempDir(), Version: "test",
		CallbackBase: "http://127.0.0.1:17867", // 环回授权回调（只用来拼 URL，测试不起真监听）
		Models:       h.models, Usage: nil,
	})
	return pn, up, p
}

// apiShim 把 /v1/models 的形状借给面板（面板只要一个 []map）。
type apiShim struct{ up *upstream.Client }

func (s *apiShim) models() []map[string]any {
	out := []map[string]any{}
	for _, mi := range s.up.Catalog() {
		out = append(out, map[string]any{"id": mi.Realm + ":" + mi.ID, "name": mi.Name})
	}
	return out
}

func call(t *testing.T, pn *Panel, method, path, body string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.RemoteAddr = "127.0.0.1:5555" // 空密钥 = 只信本机；测试请求要落在回环上
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func TestOverview(t *testing.T) {
	pn, _, _ := newTestPanel(t)
	code, body := call(t, pn, http.MethodGet, "/panel/api/overview", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if body["total"].(float64) != 1 {
		t.Errorf("total = %v", body["total"])
	}
	rows := body["accounts"].([]any)
	if len(rows) != 1 {
		t.Fatalf("账号行数 = %d", len(rows))
	}
	row := rows[0].(map[string]any)
	if row["machine_id"] != "m-1" {
		t.Errorf("machine_id 没有回显: %v", row["machine_id"])
	}
}

func TestPackages(t *testing.T) {
	pn, _, _ := newTestPanel(t)
	code, body := call(t, pn, http.MethodGet, "/panel/api/packages", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	list := body["accounts"].([]any)
	first := list[0].(map[string]any)
	if first["remain"].(float64) != 120 {
		t.Errorf("remain = %v", first["remain"])
	}
	packs := first["packages"].([]any)
	if len(packs) == 0 {
		t.Fatalf("积分构成为空")
	}
	// 口径：来源分类（每日/每月/奖励/充值）各自成包，全 0 桶不输出；
	// 前端契约字段是 remain/size，不是 credits。
	p0 := packs[0].(map[string]any)
	if p0["name"] != "每月会员积分" {
		t.Errorf("包名 = %v", p0["name"])
	}
	if p0["remain"].(float64) != 50 || p0["size"].(float64) != 50 {
		t.Errorf("包 remain/size = %v/%v", p0["remain"], p0["size"])
	}
}

func TestCheckinStatusEndpoint(t *testing.T) {
	pn, _, _ := newTestPanel(t)
	code, body := call(t, pn, http.MethodGet, "/panel/api/checkin_status", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if body["unchecked"].(float64) != 1 {
		t.Errorf("unchecked = %v（本地没签到时应为 1）", body["unchecked"])
	}
}

func TestImportPAT(t *testing.T) {
	pn, _, p := newTestPanel(t)
	code, body := call(t, pn, http.MethodPost, "/panel/api/accounts/import",
		`{"access_token":"access-1","refresh_token":"refresh-1","uid":"u-2","nickname":"导入号"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %v", code, body)
	}
	if body["action"] != "imported" {
		t.Errorf("action = %v", body["action"])
	}
	if _, ok := p.Status("u-2"); !ok {
		t.Errorf("导入后账号应进池")
	}
}

// 浏览器授权（环回回调）全链路：start 给出带 loopback 参数的授权页 → 浏览器把 code 打到
// /auth/callback → poll 换票入池。这条链路替代了原来「粘贴 office-raccoon:// 链接」的人工步骤。
func TestLoopbackLoginFlow(t *testing.T) {
	pn, _, p := newTestPanel(t)
	code, body := call(t, pn, http.MethodPost, "/panel/api/login/start", `{"realm":"cn"}`)
	if code != http.StatusOK {
		t.Fatalf("login/start status = %d body = %v", code, body)
	}
	state := fmt.Sprint(body["id"])
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(state) {
		t.Fatalf("state 应为 64 位小写 hex，得到 %q", state)
	}
	page := fmt.Sprint(body["url"])
	for _, want := range []string{
		"/code/authorize", "login_source=desktop", "desktop_callback=loopback",
		"state=" + state, "redirect=" + url.QueryEscape("http://127.0.0.1:17867/auth/callback"),
	} {
		if !strings.Contains(page, want) {
			t.Errorf("授权页 URL 缺 %q: %s", want, page)
		}
	}

	// 还没回调：poll 说没完成。
	code, body = call(t, pn, http.MethodGet, "/panel/api/login/poll?id="+state, "")
	if code != http.StatusOK || body["done"] != false {
		t.Fatalf("未回调时 poll = %d %v", code, body)
	}

	// 浏览器带着错误 state 回来：必须拒绝（否则任何人都能往池子里塞号）。
	if code, _ := call(t, pn, http.MethodGet, "/auth/callback?state="+strings.Repeat("0", 64)+"&code=abc", ""); code != http.StatusGone {
		t.Errorf("陌生 state 应 410，得到 %d", code)
	}

	// 正常回调：code 交给会话，页面回成功。
	if code, _ := call(t, pn, http.MethodGet, "/auth/callback?state="+state+"&code=ac_test", ""); code != http.StatusOK {
		t.Fatalf("回调页 status = %d", code)
	}
	// 同一个 state 只能收一次 code（防重放）。
	if code, _ := call(t, pn, http.MethodGet, "/auth/callback?state="+state+"&code=ac_test2", ""); code != http.StatusBadRequest {
		t.Errorf("重复回调应 400，得到 %d", code)
	}

	code, body = call(t, pn, http.MethodGet, "/panel/api/login/poll?id="+state, "")
	if code != http.StatusOK || body["done"] != true {
		t.Fatalf("回调后 poll = %d %v", code, body)
	}
	if body["uid"] != "u-9" {
		t.Errorf("uid = %v（授权码换票应返回 u-9）", body["uid"])
	}
	if _, ok := p.Status("u-9"); !ok {
		t.Errorf("授权完成后账号应进池")
	}
}

// 微信扫码登录：不需要任何回调地址（二维码里编的是公开网页，服务端轮询取票），
// 所以面板放在服务器/局域网也行——这是替代 127.0.0.1 环回回调的首选路径。
func TestQrLoginStart(t *testing.T) {
	pn, _, _ := newTestPanel(t)
	code, body := call(t, pn, http.MethodPost, "/panel/api/login/qr/start", `{}`)
	if code != http.StatusOK {
		t.Fatalf("qr/start status = %d body = %v", code, body)
	}
	qrCode := fmt.Sprint(body["code"])
	if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(qrCode) {
		t.Fatalf("扫码头应为 32 位小写 hex，得到 %q", qrCode)
	}
	content := fmt.Sprint(body["content"])
	if !strings.Contains(content, "/login/mp?") || !strings.Contains(content, "code="+qrCode) {
		t.Errorf("二维码内容不对: %s", content)
	}
	// 未扫码时 poll 必须说「没完成」，并带上阶段文案。
	code, body = call(t, pn, http.MethodGet, "/panel/api/login/poll?id="+qrCode, "")
	if code != http.StatusOK || body["done"] != false {
		t.Fatalf("未扫码 poll = %d %v", code, body)
	}
	if fmt.Sprint(body["qr_status"]) == "" {
		t.Errorf("poll 应带 qr_status 进度文案")
	}
}

// 二维码由面板自己画成 PNG（不依赖 CDN/前端库），且不能缓存（code 一次性）。
func TestQrLoginImage(t *testing.T) {
	pn, _, _ := newTestPanel(t)
	code, body := call(t, pn, http.MethodPost, "/panel/api/login/qr/start", `{}`)
	pngPath := fmt.Sprint(body["png"])
	if !strings.HasPrefix(pngPath, "/panel/api/login/qr.png?code=") {
		t.Fatalf("png 路径不对: %s", pngPath)
	}
	req := httptest.NewRequest(http.MethodGet, pngPath, nil)
	req.RemoteAddr = "127.0.0.1:51234" // 空密钥时面板只信本机（见 Panel.authorized）
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("qr.png status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.HasPrefix(rec.Header().Get("Cache-Control"), "no-store") {
		t.Errorf("二维码不该被缓存：Cache-Control=%q", rec.Header().Get("Cache-Control"))
	}
	if sig := rec.Body.Bytes(); len(sig) < 8 || sig[0] != 0x89 || string(sig[1:4]) != "PNG" {
		t.Errorf("不是合法 PNG（%d 字节）", rec.Body.Len())
	}
	_ = code
}

// 跨机/服务器部署：官方回调地址写死 127.0.0.1，浏览器到不了，靠粘贴链接回填。
// 三条路径都要能用：命中会话（交回轮询）、没有会话（直接换票入池）、裸授权码。
func TestLoginCodePaste(t *testing.T) {
	pn, _, p := newTestPanel(t)

	// 1) 命中本次会话：与环回回调等价，poll 能收到号。
	_, body := call(t, pn, http.MethodPost, "/panel/api/login/start", `{"realm":"cn"}`)
	state := fmt.Sprint(body["id"])
	pasted := "http://127.0.0.1:17867/auth/callback?code=ac_paste&state=" + state
	code, res := call(t, pn, http.MethodPost, "/panel/api/login/code",
		`{"url":`+strconv.Quote(pasted)+`}`)
	if code != http.StatusOK || res["action"] != "session" {
		t.Fatalf("粘贴会话链接 = %d %v", code, res)
	}
	code, res = call(t, pn, http.MethodGet, "/panel/api/login/poll?id="+state, "")
	if code != http.StatusOK || res["done"] != true || res["uid"] != "u-9" {
		t.Fatalf("粘回链接后 poll = %d %v", code, res)
	}

	// 2) 没有对应会话（换机/会话过期）：直接换票入池。
	code, res = call(t, pn, http.MethodPost, "/panel/api/login/code",
		`{"url":"office-raccoon://auth/callback?code=ac_orphan"}`)
	if code != http.StatusOK || res["action"] != "imported" {
		t.Fatalf("无会话链接 = %d %v", code, res)
	}

	// 3) 只截到授权码。
	if code, res := call(t, pn, http.MethodPost, "/panel/api/login/code", `{"url":"ac_bare"}`); code != http.StatusOK {
		t.Fatalf("裸授权码 = %d %v", code, res)
	}

	// 4) 认不出的东西要明确拒绝，不能默默当成功。
	if code, _ := call(t, pn, http.MethodPost, "/panel/api/login/code", `{"url":"https://example.com/hello"}`); code != http.StatusBadRequest {
		t.Errorf("无关链接应 400，得到 %d", code)
	}
	if len(p.List()) < 2 {
		t.Errorf("粘贴登录后账号数 = %d", len(p.List()))
	}
}

// 面板监听端口不在环回可用范围时，login/start 如实报错并指向「导入账号」。
func TestLoopbackUnsupported(t *testing.T) {
	pn, _, _ := newTestPanel(t)
	pn.cfg.CallbackBase = "" // 例如 listen 端口 <1024 或拿不到端口
	code, body := call(t, pn, http.MethodPost, "/panel/api/login/start", `{"realm":"cn"}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("status = %d body = %v", code, body)
	}
	if !strings.Contains(fmt.Sprint(body["error"]), "导入账号") {
		t.Errorf("错误文案应指向导入账号: %v", body["error"])
	}
}

// 授权回调链接导入：office-raccoon://…?code=… → 换票 → 落盘入池。
func TestImportCallbackLink(t *testing.T) {
	pn, _, p := newTestPanel(t)
	code, body := call(t, pn, http.MethodPost, "/panel/api/accounts/import",
		"office-raccoon://auth/callback?code=abc&state=st")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %v", code, body)
	}
	if body["action"] != "imported" {
		t.Errorf("action = %v", body["action"])
	}
	if len(p.List()) != 2 {
		t.Errorf("回调导入后账号数 = %d", len(p.List()))
	}
}

// refreshToken 直登：一条裸刷新令牌 → 续期 → userinfo → 落盘入池。
func TestLoginRefreshBareToken(t *testing.T) {
	pn, _, p := newTestPanel(t)
	code, body := call(t, pn, http.MethodPost, "/panel/api/login/refresh", `{"refresh_token":"drt-1","realm":"global"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %v", code, body)
	}
	if body["uid"] != "u-1" {
		t.Errorf("uid = %v", body["uid"])
	}
	if _, ok := p.Status("u-1"); !ok {
		t.Errorf("账号应进池")
	}
}

func TestLoginRefreshRejectsGarbage(t *testing.T) {
	pn, _, _ := newTestPanel(t)
	code, _ := call(t, pn, http.MethodPost, "/panel/api/login/refresh", `{"refresh_token":""}`)
	if code != http.StatusBadRequest {
		t.Errorf("空 refreshToken 应 400，得到 %d", code)
	}
}

// TestReconcileCredits 积分对账：上游扣费流水的 event_name 里带着我们出站的 X-RACCOON-Title
// （「任务消耗 · req-xxxx」），面板据此把真实扣分回填到那条请求记录上。
func TestReconcileCredits(t *testing.T) {
	pn, up, p := newTestPanel(t)
	_ = up
	rec := reqlog.New(reqlog.Config{})
	pn.cfg.RequestLog = rec
	rec.Begin()
	rec.Record(reqlog.Event{RequestID: "req-abcdef0123456789", Model: "cn:raccoon-8c4485",
		Status: 200, OK: true, TotalTokens: 40})

	// 指向上游假流水：一条带我们 rid 的消费行 + 一条无关的发放行。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"data":{"items":[
			{"biz_type":"desktop","event_name":"任务消耗 · req-abcdef0123456789","points":-2,
			 "created_at":"2026-10-21T06:25:19+08:00"},
			{"biz_type":"daily_grant","event_name":"每日积分发放","points":300,
			 "created_at":"2026-10-21T00:25:04+08:00"}]}}`)
	}))
	defer srv.Close()
	pn.cfg.Upstream.SetAPIHosts(srv.URL, srv.URL)

	if n := pn.reconcileCredits(); n != 1 {
		t.Fatalf("对账回填 %d 条，期望 1 条", n)
	}
	got := rec.Snapshot()
	found := false
	for _, e := range got.Recent {
		if e.RequestID == "req-abcdef0123456789" {
			found = true
			if !e.HasCredit || e.Credit != 2 {
				t.Errorf("回填后 credit_known=%v credit=%v，期望 true/2", e.HasCredit, e.Credit)
			}
		}
	}
	if !found {
		t.Fatal("请求记录里找不到那条 rid")
	}
	_ = p
}

// TestRequestIDFromBillEvent 只认我们自己的 rid 形态，别的标题不能被误当请求 id。
func TestRequestIDFromBillEvent(t *testing.T) {
	cases := map[string]string{
		"任务消耗 · req-abcdef0123456789":    "req-abcdef0123456789",
		"任务消耗 · 1+1=几? 请你直接回答":           "",
		"桌面端 LLM 对话消耗":                   "",
		"任务消耗 · req-0011aabbccddeeff 尾巴": "req-0011aabbccddeeff",
	}
	for in, want := range cases {
		if got := requestIDFromBillEvent(in); got != want {
			t.Errorf("requestIDFromBillEvent(%q) = %q，期望 %q", in, got, want)
		}
	}
}
