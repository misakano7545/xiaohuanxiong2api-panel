
package scheduler

import (
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

// TestCheckinPendingWhenBillLagging 流水尚未落库时不该判失败。
//
// 场景（2026-11-10 实测复现）：上游 00:00 发了每日积分，但 bills 里
// 今天那条 daily_grant 的 created_at 要晚几十分钟到几小时才出现。
// 旧逻辑这时判「今天没发」→ checkinErr=「签到复核未通过」→ 记 5 分钟冷却，
// 面板挂一句吓人的失败。实际积分早到账，补签轮次本来就会签上。
func TestCheckinPendingWhenBillLagging(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	yesterday := time.Now().In(loc).AddDate(0, 0, -1).Format("2006-01-02")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/balance":
			_, _ = io.WriteString(w, `{"code":0,"data":{"available_points":9000,"daily_points":600,"reward_points":8400}}`)
		case "/bills":
			// 只有昨天的 daily_grant，今天这条还没落库。
			_, _ = io.WriteString(w, `{"code":0,"data":{"items":[
				{"biz_type":"daily_grant","points":300,"grant_at":"`+yesterday+`T00:00:00+08:00","created_at":"`+yesterday+`T00:00:00+08:00"},
				{"biz_type":"reward_grant","points":3000,"created_at":"2026-10-01T05:46:03+08:00"}]}}`)
		case "/user_info":
			_, _ = io.WriteString(w, `{"code":0,"data":{"id":"u-lag","name":"老号","created_at":"2026-10-01T10:00:00+08:00"}}`)
		case "/api/web/desktop/v1/login/points/grant":
			_, _ = io.WriteString(w, `{"code":0,"data":{"granted":false,"popup":null}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	up := upstream.New()
	up.SetAPIHosts(srv.URL, srv.URL)
	up.SetAgentHosts(srv.URL, srv.URL)
	p := pool.New("")
	p.Add(&auth.Auth{Data: auth.Data{
		AccessToken: "dt-lag", ExpiresAt: time.Now().Add(48 * time.Hour).UnixMilli(),
		MachineID: "m-lag", UID: "u-lag",
	}})
	s := New(Config{Pool: p, Upstream: up, CheckinEnabled: true, KeepaliveEnabled: true})

	res, err := s.CheckinUID("u-lag")
	if err != nil {
		t.Fatalf("流水延迟不该报错: %v（status=%q reason=%q）", err, res.Status, res.Reason)
	}
	if res.Status != "pending" {
		t.Fatalf("状态 = %q，期望 pending（reason=%q）", res.Status, res.Reason)
	}
	if !strings.Contains(res.Reason, "积分流水") {
		t.Errorf("pending 原因应说明是流水未到：%q", res.Reason)
	}
	if p.CheckinCooling("u-lag") {
		t.Error("流水延迟不该给账号记签到冷却")
	}
	if st, ok := p.Status("u-lag"); ok && st.CheckinReason != "" {
		t.Errorf("不该在面板上留下签到失败原因：%q", st.CheckinReason)
	}
}
