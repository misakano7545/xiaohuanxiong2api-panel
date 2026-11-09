package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
)

// TestBillsQuerySurvivesConsumptionFlood 当天消费把窗口挤满时，签到判据不能失效。
//
// 复现（2026-11-10 实测）：用 type=all 时上游只返回最近 100 条，重度使用一天
// 95+ 条 -1 消费就把当天的 daily_grant 挤出窗口 → Today/Ever/LastDay 全查空
// → 复核误判「没签到」，甚至会错走「上游未开通每日积分」分支。
// 改为 type=income 后只返回发放流水，消费再多也挤不掉。
func TestBillsQuerySurvivesConsumptionFlood(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	today := time.Now().In(loc).Format("2006-01-02")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bills" {
			http.NotFound(w, r)
			return
		}
		q, _ := url.ParseQuery(r.URL.RawQuery)
		// 上游语义：type=income 只给发放；type=all 按时间倒序截断到 limit。
		all := []map[string]any{}
		if q.Get("type") == "income" {
			all = append(all, map[string]any{
				"biz_type": "daily_grant", "points": 300,
				"grant_at": today + "T00:00:00+08:00", "created_at": today + "T00:05:00+08:00",
			})
		} else {
			for i := 0; i < 100; i++ { // 当天 100 条消费占满窗口
				all = append(all, map[string]any{
					"biz_type": "desktop", "points": -1, "created_at": today + "T10:00:00+08:00",
				})
			}
			all = append(all, map[string]any{
				"biz_type": "daily_grant", "points": 300,
				"grant_at": today + "T00:00:00+08:00", "created_at": today + "T00:05:00+08:00",
			})
			if len(all) > 100 {
				all = all[:100]
			}
		}
		b, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"items": all}})
		_, _ = io.WriteString(w, string(b))
	}))
	defer srv.Close()

	up := New()
	up.SetAPIHosts(srv.URL, srv.URL)
	up.SetAgentHosts(srv.URL, srv.URL)
	a := &auth.Auth{Data: auth.Data{
		AccessToken: "dt-flood", ExpiresAt: time.Now().Add(48 * time.Hour).UnixMilli(),
		UID: "u-flood", MachineID: "m-flood",
	}}
	d, err := up.DailyGrantDetail(a)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if !d.Today {
		t.Errorf("当天 daily_grant 已发放却判 Today=false（窗口被消费挤爆）")
	}
	if !d.Ever || d.LastDay != today {
		t.Errorf("Ever=%v LastDay=%q，应为 true / %q", d.Ever, d.LastDay, today)
	}
}
