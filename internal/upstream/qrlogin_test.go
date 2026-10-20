package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 扫码登录的状态机：pending/logging 都不算完成，success 才换票，canceled 是终态。
// 上游偶发「success 但不给 access_token」，那种必须当没完成——否则会拿空票据卡死。
func TestQrLoginPollStates(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantDone bool
		wantErr  bool
		wantUID  string
	}{
		{"pending", `{"code":0,"data":{"status":"pending"}}`, false, false, ""},
		{"logging", `{"code":0,"data":{"status":"logging","expired_at":"2026-10-21T07:00:00+08:00"}}`, false, false, ""},
		{"canceled", `{"code":0,"data":{"status":"canceled"}}`, false, true, ""},
		{"success 但没票", `{"code":0,"data":{"status":"success"}}`, false, false, ""},
		{"success", `{"code":0,"data":{"status":"success","access_token":"at-1","refresh_token":"rt-1"}}`, true, false, "u-9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case QrCodePath:
					// 请求体必须是 {"qrcode_code": "<我们的 code>"}
					raw, _ := io.ReadAll(r.Body)
					if !strings.Contains(string(raw), "qrcode_code") {
						t.Errorf("轮询没带 qrcode_code: %s", raw)
					}
					_, _ = io.WriteString(w, c.body)
				case UserInfoPath:
					_, _ = io.WriteString(w, `{"code":0,"data":{"id":"u-9","name":"扫码号"}}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			cl := New()
			cl.SetAgentHosts(srv.URL, srv.URL) // auth 域（user_info + 扫码轮询）
			cl.SetAPIHosts(srv.URL, srv.URL)
			q, err := NewQrLogin(cl, "machine-1")
			if err != nil {
				t.Fatal(err)
			}
			if len(q.QRCode()) != 32 {
				t.Fatalf("扫码头应是 32 位 hex，得到 %q", q.QRCode())
			}
			if !strings.Contains(q.QRContent(), "/login/mp?") || !strings.Contains(q.QRContent(), q.QRCode()) {
				t.Fatalf("二维码内容不对: %s", q.QRContent())
			}
			creds, done, err := q.Poll(nil)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望错误，得到 done=%v creds=%v", done, creds)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if done != c.wantDone {
				t.Fatalf("done = %v，期望 %v", done, c.wantDone)
			}
			if c.wantDone {
				if creds == nil || creds.UID != c.wantUID || creds.JWT() != "at-1" {
					t.Fatalf("凭据不对: %+v", creds)
				}
				// 注意：machine_id 在小浣熊里是基线遗留字段（设备指纹由 pool 的 ug 派生），
				// 这里不要求它被填——存进文件里反而是假信息。
				if creds.Nickname != "扫码号" {
					t.Errorf("昵称应以 user_info 为准，得到 %q", creds.Nickname)
				}
			}
		})
	}
}

// 轮询异常（网络/畸形响应）必须降级成「没完成」，不能中断整个登录流程。
func TestQrLoginPollFailuresArePending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, `{"code":1,"message":"boom"}`)
	}))
	defer srv.Close()
	cl := New()
	cl.SetAgentHosts(srv.URL, srv.URL)
	q, _ := NewQrLogin(cl, "")
	creds, done, err := q.Poll(nil)
	if err != nil || done || creds != nil {
		t.Fatalf("上游 500 时应安静地报没完成，得到 done=%v err=%v", done, err)
	}
}
