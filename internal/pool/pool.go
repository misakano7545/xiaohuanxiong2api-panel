// Package pool 是账号池：选号、冷却、熔断计数。状态只在内存里（重启即清零）——
// 账号的 enabled / token 才落盘，冷却是一次运行期的事实，没必要跨重启保留。
package pool

import (
	"sort"
	"sync"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
)

// breakerStrikes 是连续失败几次后进入冷却。
const breakerStrikes = 3

type entry struct {
	a        *auth.Auth
	cooling  time.Time
	reason   string
	errs     int
	inflight int
	lastUsed time.Time
}

// Pool 是线程安全的账号池。
type Pool struct {
	mu       sync.Mutex
	byUID    map[string]*entry
	cooldown time.Duration
	now      func() time.Time
}

// Status 是面板展示用的一行。
type Status struct {
	UID        string `json:"uid"`
	Name       string `json:"name"`
	UserID     string `json:"user_id,omitempty"`
	Tail       string `json:"token_tail"`
	Enabled    bool   `json:"enabled"`
	Cooling    bool   `json:"cooling"`
	Until      string `json:"cooling_until,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Errors     int    `json:"errors"`
	InFlight   int    `json:"in_flight"`
	Priority   int    `json:"priority"`
	ExpiresAt  int64  `json:"expires_at,omitempty"`
	Credit     int64  `json:"credit"`
	CreditAt   string `json:"credit_at,omitempty"`
	CheckedIn  string `json:"checked_in,omitempty"`
	HasRefresh bool   `json:"has_refresh"`
}

// New 用已有账号构造池。
func New(accounts []*auth.Auth, cooldown time.Duration) *Pool {
	p := &Pool{byUID: map[string]*entry{}, cooldown: cooldown, now: time.Now}
	for _, a := range accounts {
		p.byUID[a.UID] = &entry{a: a}
	}
	return p
}

// SetNow 注入时钟（测试用）。
func (p *Pool) SetNow(f func() time.Time) {
	p.mu.Lock()
	p.now = f
	p.mu.Unlock()
}

// Add 加入（同 uid 覆盖凭证，保留冷却状态）。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// Get 按 uid 取账号。
func (p *Pool) Get(uid string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// Remove 摘掉账号并返回它（调用方负责删文件）。
func (p *Pool) Remove(uid string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	delete(p.byUID, uid)
	return e.a
}

// Accounts 返回全部账号（顺序稳定，供后台任务遍历）。
func (p *Pool) Accounts() []*auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*auth.Auth, 0, len(p.byUID))
	for _, e := range p.byUID {
		out = append(out, e.a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

// Pick 选一个可用账号：enabled、不在冷却、未被本请求试过；priority 小的优先，
// 同 priority 轮流（最久未用的先出）。
func (p *Pool) Pick(exclude map[string]bool) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var best *entry
	for _, e := range p.byUID {
		if exclude[e.a.UID] || !e.a.Enabled || now.Before(e.cooling) {
			continue
		}
		if best == nil {
			best = e
			continue
		}
		if e.a.Priority != best.a.Priority {
			if e.a.Priority < best.a.Priority {
				best = e
			}
			continue
		}
		if e.lastUsed.Before(best.lastUsed) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	best.lastUsed = now
	best.inflight++
	return best.a
}

// Release 交还在途名额。
func (p *Pool) Release(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && e.inflight > 0 {
		e.inflight--
	}
}

// Cooldown 让账号冷却一段时间。
func (p *Pool) Cooldown(uid string, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	e.cooling = p.now().Add(d)
	e.reason = reason
}

// NoteError 记一次失败；连续失败到阈值就按池的冷却时长挂起。
func (p *Pool) NoteError(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	e.errs++
	if e.errs >= breakerStrikes {
		e.cooling = p.now().Add(p.cooldown)
		e.reason = "连续失败 " + itoa(e.errs) + " 次"
		e.errs = 0
	}
}

// NoteSuccess 清连续失败计数。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errs = 0
	}
}

// Disable 停用一个账号（落盘由调用方做）。
func (p *Pool) Disable(uid, reason string) {
	if a := p.Get(uid); a != nil {
		a.Enabled = false
	}
	p.mu.Lock()
	if e, ok := p.byUID[uid]; ok {
		e.reason = reason
	}
	p.mu.Unlock()
}

// Enable 恢复账号。
func (p *Pool) Enable(uid string) {
	if a := p.Get(uid); a != nil {
		a.Enabled = true
	}
	p.ClearCooling(uid)
}

// ClearCooling 清冷却与失败计数。
func (p *Pool) ClearCooling(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.cooling = time.Time{}
		e.reason = ""
		e.errs = 0
	}
}

// CooldownDuration 返回池的默认冷却时长。
func (p *Pool) CooldownDuration() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cooldown
}

// Statuses 返回面板展示行。
func (p *Pool) Statuses() []Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := make([]Status, 0, len(p.byUID))
	for _, e := range p.byUID {
		snap := e.a.Snapshot()
		s := Status{
			UID:        snap.UID,
			Name:       snap.Name,
			UserID:     snap.UserID,
			Tail:       auth.TokenTail(snap.AccessToken),
			Enabled:    snap.Enabled,
			Reason:     e.reason,
			Errors:     e.errs,
			InFlight:   e.inflight,
			Priority:   snap.Priority,
			ExpiresAt:  snap.ExpiresAt,
			Credit:     snap.Credit,
			CreditAt:   snap.CreditAt,
			CheckedIn:  snap.CheckedIn,
			HasRefresh: snap.RefreshToken != "",
		}
		if now.Before(e.cooling) {
			s.Cooling = true
			s.Until = e.cooling.Format(time.RFC3339)
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

// Available 报告是否有可用的号（健康检查用）。
func (p *Pool) Available() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for _, e := range p.byUID {
		if e.a.Enabled && !now.Before(e.cooling) {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
