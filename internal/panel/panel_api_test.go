package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/pool"
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
			// 按令牌回身份：access-1 是 TestImportPAT 导入号的票，别的按 u-1。
			if r.Header.Get("Authorization") == "Bearer access-1" {
				_, _ = io.WriteString(w, `{"code":0,"data":{"id":"u-2","name":"导入号"}}`)
			} else {
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
		Models: h.models, Usage: nil,
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
	if packs[0].(map[string]any)["name"] != "可用积分" {
		t.Errorf("包名 = %v", packs[0].(map[string]any)["name"])
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

// 小浣熊没有设备码授权：login/start 如实报错，用户改走「导入账号」。
func TestDeviceLoginUnsupported(t *testing.T) {
	pn, _, _ := newTestPanel(t)
	code, body := call(t, pn, http.MethodPost, "/panel/api/login/start", `{"realm":"cn"}`)
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d", code)
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
