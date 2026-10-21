package scheduler

import (
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

// TestCheckinFlow 跑一次真实签到链路：status（CLAIMABLE）→ claim → 复核（CLAIMED）。
func TestCheckinFlow(t *testing.T) {
	claimed := false
	mobileCalls := 0
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
		case "/api/web/mobile/v1/login/points/grant":
			mobileCalls++
			// 手机端只认 App 身份头；桌面那套带 client-name 的头说明走错了端点口径。
			if got := r.Header.Get("x-client-platform"); got != "app-android" {
				t.Errorf("手机端首登奖励的 x-client-platform = %q，期望 app-android", got)
			}
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
	if mobileCalls != 1 {
		t.Errorf("手机端首登奖励调用 %d 次，期望 1 次", mobileCalls)
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

// TestCheckinSkipsAccountWithoutDailyGrant 上游从没给这个号发过每日积分（真实例：
// 7692045 注册 4 天，流水里只有 reward_grant，daily_points 恒 0）：claim 没意义、
// 复核必然 false，但这不是签到失败——不能每轮刷假失败、更不能记冷却。
func TestCheckinSkipsAccountWithoutDailyGrant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/balance":
			_, _ = io.WriteString(w, `{"code":0,"data":{"available_points":5968,"daily_points":0,"reward_points":5968}}`)
		case "/bills":
			// 只有注册礼包与桌面端登录奖励，一条 daily_grant 都没有。
			_, _ = io.WriteString(w, `{"code":0,"data":{"items":[
				{"biz_type":"reward_grant","points":3000,"created_at":"2026-10-21T15:22:15+08:00"},
				{"biz_type":"reward_grant","points":3000,"created_at":"2026-10-17T23:23:02+08:00"}]}}`)
		case "/user_info":
			// 注册于 4 天前：不是「当天新号」，pending 那条兜底不适用。
			_, _ = io.WriteString(w, `{"code":0,"data":{"id":"u-7","name":"老号但没日发","created_at":"2026-10-17T23:23:02+08:00"}}`)
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
		AccessToken: "dt-old", ExpiresAt: time.Now().Add(48 * time.Hour).UnixMilli(),
		MachineID: "m-7", UID: "u-7",
	}})
	s := New(Config{Pool: p, Upstream: up, CheckinEnabled: true, KeepaliveEnabled: true})

	res, err := s.CheckinUID("u-7")
	if err != nil {
		t.Fatalf("未开通每日积分不该报错: %v（status=%q reason=%q）", err, res.Status, res.Reason)
	}
	if res.Status != "skipped" {
		t.Fatalf("状态 = %q，期望 skipped（reason=%q）", res.Status, res.Reason)
	}
	if !strings.Contains(res.Reason, "daily_grant") {
		t.Errorf("skipped 原因没说明白：%q", res.Reason)
	}
	if p.CheckinCooling("u-7") {
		t.Error("未开通每日积分不该记签到冷却")
	}
}

// TestCheckinPendingForNewAccount 当天新注册的号：上游 00:00 那批每日积分还没发，
// bills 里没有 daily_grant。此时 claim 会拿到「登录奖励」，但复核必然是 false——
// 这不能记成签到失败（否则每分钟重试、还给它记签到冷却）。
func TestCheckinPendingForNewAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/balance":
			_, _ = io.WriteString(w, `{"code":0,"data":{"available_points":6000,"reward_points":6000}}`)
		case "/bills":
			// 只有注册礼包与登录奖励，没有 daily_grant。
			_, _ = io.WriteString(w, `{"code":0,"data":{"items":[
				{"biz_type":"reward_grant","points":3000,"created_at":"2026-10-21T05:46:03+08:00"},
				{"biz_type":"reward_grant","points":3000,"created_at":"2026-10-21T05:34:24+08:00"}]}}`)
		case "/user_info":
			today := time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02T15:04:05")
			_, _ = io.WriteString(w, `{"code":0,"data":{"id":"u-9","name":"新号","created_at":"`+today+`+08:00"}}`)
		case "/api/web/desktop/v1/login/points/grant":
			_, _ = io.WriteString(w, `{"code":0,"data":{"granted":true,"popup":null}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	up := upstream.New()
	up.SetAPIHosts(srv.URL, srv.URL)
	// user_info 走 agent(auth) 域，SetAPIHosts 只覆盖 points/origin，必须单独设。
	up.SetAgentHosts(srv.URL, srv.URL)
	p := pool.New("")
	p.Add(&auth.Auth{Data: auth.Data{
		AccessToken: "dt-new", ExpiresAt: time.Now().Add(48 * time.Hour).UnixMilli(),
		MachineID: "m-9", UID: "u-9",
	}})
	s := New(Config{Pool: p, Upstream: up, CheckinEnabled: true, KeepaliveEnabled: true})

	res, err := s.CheckinUID("u-9")
	if err != nil {
		t.Fatalf("新号当天不应报错: %v（status=%q）", err, res.Status)
	}
	if res.Status != "pending" {
		t.Fatalf("状态 = %q，期望 pending（reason=%q）", res.Status, res.Reason)
	}
	if res.Reason == "" || !strings.Contains(res.Reason, "次日 00:00") {
		t.Errorf("pending 原因没说清：%q", res.Reason)
	}
	if p.CheckinCooling("u-9") {
		t.Error("pending 不该给账号记签到冷却")
	}
}
