package scheduler

import (
	"context"
	"testing"
	"time"
)

// TestWakeBroadcast 广播语义：一次 ApplySchedule 必须叫醒所有等待者。
//
// 这条是被实测 bug 逼出来的：早先用「容量 1 的 chan + 非阻塞发」，主循环/余额循环/
// 补签循环三处都在等同一个 token，谁先抢到就吞掉——面板连续两次保存，第二次就没有
// 任何循环重排（日志里只有一行「排程已热更新」）。改成 close+换代后每个等待者都能收到。
func TestWakeBroadcast(t *testing.T) {
	s := New(Config{})
	first, second := s.wakeup(), s.wakeup()

	s.ApplySchedule(false, nil, false, nil, 0)
	for i, ch := range []<-chan struct{}{first, second} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("第 %d 个等待者没被叫醒——广播退化成了单播", i+1)
		}
	}

	// 换代之后重新取的通道还能收到下一次广播（不能等已关闭的旧 chan）。
	third := s.wakeup()
	s.ApplySchedule(true, []int{9}, true, []int{3}, time.Minute)
	select {
	case <-third:
	case <-time.After(time.Second):
		t.Fatal("换代后的广播丢了")
	}
	// 旧通道保持关闭（等待者不会卡死）。
	select {
	case <-first:
	default:
		t.Fatal("旧一代通道应当是已关闭状态")
	}
}

// TestApplyScheduleHot 排程热改：面板保存配置后不重启就要生效。
//
// 覆盖三件事：
//  1. 两个任务都关着时 Run 停在 wake 上（既不退出也不空转）；
//  2. ApplySchedule 立刻反映到 Status（面板「下次签到」马上跟着变）；
//  3. wake 容量 1 + 非阻塞发：连续改排程不会把面板请求卡住。
func TestApplyScheduleHot(t *testing.T) {
	// runBalanceLoop 用到的周期给 0：这个用例不测余额刷新，别起真 ticker。
	sch := New(Config{CheckinEnabled: false, KeepaliveEnabled: false, BalanceRefreshInterval: 0})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sch.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	// 都关掉：Run 应该躺在 wake 上，而不是直接返回。
	select {
	case <-done:
		t.Fatal("两个任务都关时 Run 不该退出——否则之后热开启就永远不会触发")
	case <-time.After(50 * time.Millisecond):
	}

	sch.ApplySchedule(true, []int{9}, true, []int{3}, time.Minute)
	st := sch.Status()
	if !st.CheckinEnabled || !st.KeepaliveEnabled {
		t.Fatalf("开关没热生效: %+v", st)
	}
	if len(st.CheckinHours) != 1 || st.CheckinHours[0] != 9 {
		t.Fatalf("签到时点没热生效: %v", st.CheckinHours)
	}
	if st.CheckinNext == "" || st.KeepaliveNext == "" {
		t.Fatalf("下次时点没算出来: %+v", st)
	}
	if st.BalanceIntervalSec != 60 {
		t.Fatalf("余额刷新周期没热生效: %ds", st.BalanceIntervalSec)
	}

	// 连改两次：close+换代，不阻塞。
	freed := make(chan struct{})
	go func() {
		sch.ApplySchedule(false, nil, true, []int{4}, 0)
		sch.ApplySchedule(true, []int{10}, false, nil, 2*time.Minute)
		close(freed)
	}()
	select {
	case <-freed:
	case <-time.After(2 * time.Second):
		t.Fatal("ApplySchedule 阻塞了——广播必须非阻塞")
	}

	// 改完 Run 仍活着（唤醒后重排，不是退出）。
	select {
	case <-done:
		t.Fatal("改排程后 Run 退出了")
	default:
	}
	if st := sch.Status(); !st.CheckinEnabled || st.KeepaliveEnabled || st.BalanceIntervalSec != 120 {
		t.Fatalf("第二次改排程没生效: %+v", st)
	}
}
