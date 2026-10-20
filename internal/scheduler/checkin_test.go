package scheduler

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/pool"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/upstream"
)

// TestCheckinFlow 跑一次真实签到链路：status（CLAIMABLE）→ claim → 复核（CLAIMED）。
func TestCheckinFlow(t *testing.T) {
	claimed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/balance":
			// 领取前后余额不同：签到链路要能看到「领取后复核」拿到的是新值。
			remain := 200
			if claimed {
				remain = 300
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"available_points":`+fmt.Sprint(remain)+`}}`)
		case "/api/web/office/v3/setting_info":
			claimed = true
			_, _ = io.WriteString(w, `{"code":0,"data":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	up := upstream.New()
	up.SetAPIHosts(srv.URL, srv.URL)
	p := pool.New("")
	p.Add(&auth.Auth{Data: auth.Data{
		AccessToken: "dt", RefreshToken: "drt",
		ExpiresAt: time.Now().Add(48 * time.Hour).UnixMilli(),
		MachineID: "m-1", UID: "u-1",
	}})
	s := New(Config{Pool: p, Upstream: up, CheckinEnabled: true, KeepaliveEnabled: true})

	res, err := s.CheckinUID("u-1")
	if err != nil {
		t.Fatalf("CheckinUID 出错: %v", err)
	}
	if res.Status != "ok" {
		t.Fatalf("签到状态 = %q（%s）", res.Status, res.Reason)
	}
	if !claimed {
		t.Errorf("没有真的调用领取接口")
	}
	if st, ok := p.Status("u-1"); !ok || st.Credits != 300 {
		t.Errorf("签到后余额未回写: %+v", st)
	}

	// 再签一次：本地已记今天签过 → 应为 already，而不是失败。
	res2, err := s.CheckinUID("u-1")
	if err != nil {
		t.Fatalf("第二次 CheckinUID 出错: %v", err)
	}
	if res2.Status != "already" {
		t.Errorf("第二次签到状态 = %q", res2.Status)
	}
}
