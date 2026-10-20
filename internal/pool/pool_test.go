package pool

import (
	"testing"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
)

func acct(uid string, priority int) *auth.Auth {
	a := auth.New("access-"+uid, "refresh-"+uid, uid)
	a.UID = uid
	a.Priority = priority
	return a
}

func TestPickRespectsPriorityAndExclusion(t *testing.T) {
	high, low := acct("high", 0), acct("low", 5)
	p := New([]*auth.Auth{high, low}, time.Minute)

	if got := p.Pick(nil); got == nil || got.UID != "high" {
		t.Fatalf("应先选 priority 小的: %v", got)
	}
	if got := p.Pick(map[string]bool{"high": true}); got == nil || got.UID != "low" {
		t.Fatalf("应跳过已试过的号: %v", got)
	}
	if got := p.Pick(map[string]bool{"high": true, "low": true}); got != nil {
		t.Fatalf("全被排除时应返回 nil: %v", got)
	}
}

func TestCooldownHidesAccount(t *testing.T) {
	a, other := acct("a", 0), acct("b", 1)
	p := New([]*auth.Auth{a, other}, time.Minute)
	now := time.Unix(1_700_000_000, 0)
	p.SetNow(func() time.Time { return now })

	p.Cooldown("a", time.Minute, "上游限流")
	if got := p.Pick(nil); got == nil || got.UID != "b" {
		t.Fatalf("冷却中的号不该被选: %v", got)
	}
	now = now.Add(2 * time.Minute)
	if got := p.Pick(nil); got == nil || got.UID != "a" {
		t.Fatalf("冷却结束后应重新可选: %v", got)
	}
}

func TestDisableAndBreaker(t *testing.T) {
	a := acct("a", 0)
	p := New([]*auth.Auth{a}, time.Minute)
	now := time.Unix(1_700_000_000, 0)
	p.SetNow(func() time.Time { return now })

	p.Disable("a", "手动")
	if p.Pick(nil) != nil {
		t.Fatalf("停用的号不该被选")
	}
	p.Enable("a")
	if p.Pick(nil) == nil {
		t.Fatalf("启用后应可选")
	}
	for i := 0; i < breakerStrikes; i++ {
		p.NoteError("a")
	}
	if got := p.Pick(nil); got != nil {
		t.Fatalf("连续失败到阈值应进入冷却: %v", got)
	}
	if p.Available() {
		t.Fatalf("唯一账号冷却中时 Available 应为 false")
	}
	p.NoteSuccess("a") // 冷却不清，只是清零计数
	p.ClearCooling("a")
	if !p.Available() {
		t.Fatalf("清冷却后应可用")
	}
}
