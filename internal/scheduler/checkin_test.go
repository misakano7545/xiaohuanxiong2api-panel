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
		case "/bills":
			// 复核口径：流水里今天已有 daily_grant 才算「签到成功」。
			if claimed {
				today := time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
				_, _ = io.WriteString(w, `{"code":0,"data":{"items":[{"biz_type":"daily_grant",`+
					`"points":300,"grant_at":"`+today+`T00:00:00+08:00"}]}}`)
			} else {
				_, _ = io.WriteString(w, `{"code":0,"data":{"items":[]}}`)
			}
		case "/api/web/desktop/v1/login/points/grant":
			claimed = true
			_, _ = io.WriteString(w, `{"code":0,"data":{"granted":true,"popup":null}}`)
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
