package scheduler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/misakano7545/qoder2api-panel/internal/auth"
	"github.com/misakano7545/qoder2api-panel/internal/pool"
	"github.com/misakano7545/qoder2api-panel/internal/upstream"
)

// TestCheckinFlow 跑一次真实签到链路：status（CLAIMABLE）→ claim → 复核（CLAIMED）。
func TestCheckinFlow(t *testing.T) {
	claimed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sash/api/v1/me/campaigns":
			status := "CLAIMABLE"
			if claimed {
				status = "CLAIMED"
			}
			_, _ = io.WriteString(w, `{"campaigns":[{"campaignId":"c-1","actionType":"CLAIM_BENEFIT","claimStatus":"`+status+`"}]}`)
		case "/sash/api/v1/me/campaigns/c-1/claim":
			claimed = true
			_, _ = io.WriteString(w, `{"status":"CLAIMED","benefit":{"amount":100}}`)
		case "/api/v2/quota/usage":
			_, _ = io.WriteString(w, `{"userQuota":{"remaining":200,"total":200,"used":0},"expiresAt":1900000000000}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	up := upstream.New()
	up.SetAPIHosts(srv.URL, srv.URL)
	p := pool.New("")
	p.Add(&auth.Auth{
		AccessToken: "dt", RefreshToken: auth.PackRefresh("drt", "u-1", "m-1"),
		ExpiresAt: time.Now().Add(48 * time.Hour).UnixMilli(), RealmID: auth.RealmGlobal,
		MachineID: "m-1", UID: "u-1",
	})
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
	if st, ok := p.Status("u-1"); !ok || st.Credits != 200 {
		t.Errorf("签到后余额未回写: %+v", st)
	}

	// 再签一次：上游已是 CLAIMED → 应为 already，而不是失败。
	res2, err := s.CheckinUID("u-1")
	if err != nil {
		t.Fatalf("第二次 CheckinUID 出错: %v", err)
	}
	if res2.Status != "already" {
		t.Errorf("第二次签到状态 = %q", res2.Status)
	}
}
