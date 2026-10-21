// Package scheduler 定时任务：每日签到 + token 保活 + 周期余额刷新。
// 签到成功后重新查积分，积分 > 0 的冷却账号自动解冻（只在这一条路径上解冻，见
// RunBalanceRefresh 的说明）。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/url"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/pool"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/upstream"
)

// Config 调度器依赖。
//
// 时点集合与开关在 New 时定下，之后可以整体热替换（见 ApplySchedule）：面板保存配置时
// 调它，Run 里的等待会被唤醒并按新排程重算下一次时点，不需要重启进程。
type Config struct {
	Pool     *pool.Pool
	Upstream *upstream.Client

	CheckinHours   []int // 每日签到时点，默认 [9]
	KeepaliveHours []int // token 保活时点，默认 [3]

	CheckinEnabled   bool // 默认 true
	KeepaliveEnabled bool // 默认 true

	// BalanceRefreshInterval > 0 时按它周期刷余额（0 = 不启动）。
	BalanceRefreshInterval time.Duration
}

// 面板签到可辨识的账号状态。
var (
	ErrNotFound = errors.New("account not found")
	ErrDisabled = errors.New("account disabled")
)

// Scheduler 调度器。
type Scheduler struct {
	cfg Config
	mu  sync.RWMutex
	// wake 排程变更广播：ApplySchedule 关掉旧 chan 并换一个新的，所有等待者同时醒来。
	// 不用「带缓冲的 chan + 非阻塞发」：主循环、余额循环、补签循环三处都在等它，
	// 单 token 会被其中一个抢走、另两个白等一轮（实测第二次保存没有重排）。
	wakeMu sync.Mutex
	wakeCh chan struct{}
	// checkinLock / refreshLock 单飞锁：整点时点、每小时补签 ticker、面板按钮、启动补跑
	// 都可能同时进来。同一账号被两路送进 claim = 秒级二次 claim，而上游原注释说
	// 「秒级内的第二次 claim 会延长限流窗口」——所以同一任务同时只允许一轮。
	checkinLock sync.Mutex
	refreshLock sync.Mutex
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{3}
	}
	return &Scheduler{cfg: cfg, wakeCh: make(chan struct{})}
}

// wakeup 取当前这一代的广播通道（每轮循环都要重新取，否则换代后等的还是已关闭的旧 chan）。
func (s *Scheduler) wakeup() <-chan struct{} {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	return s.wakeCh
}

// ApplySchedule 热替换排程（面板保存配置后调用），并广播给运行中的循环。
//
// 只换排程段：Pool/Upstream 是进程级依赖，不随配置改。时点落盘前已由 config 层校验
// （0-23、关闭的任务允许时点非法），这里不再二次校验。
func (s *Scheduler) ApplySchedule(checkinEnabled bool, checkinHours []int,
	keepaliveEnabled bool, keepaliveHours []int, balanceInterval time.Duration) {
	s.mu.Lock()
	s.cfg.CheckinEnabled = checkinEnabled
	s.cfg.CheckinHours = append([]int(nil), checkinHours...)
	s.cfg.KeepaliveEnabled = keepaliveEnabled
	s.cfg.KeepaliveHours = append([]int(nil), keepaliveHours...)
	s.cfg.BalanceRefreshInterval = balanceInterval
	s.mu.Unlock()
	s.wakeMu.Lock()
	close(s.wakeCh)
	s.wakeCh = make(chan struct{})
	s.wakeMu.Unlock()
}

// allHours 当前启用的全部触发时点（两个任务都关掉时为空 → 主循环无事可做）。
func (s *Scheduler) allHours() []int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []int
	if s.cfg.CheckinEnabled {
		out = append(out, s.cfg.CheckinHours...)
	}
	if s.cfg.KeepaliveEnabled {
		out = append(out, s.cfg.KeepaliveHours...)
	}
	return out
}

// Status 排程现状：面板显示「下次签到/保活几点」。时点是本地小时，关掉的任务不排。
type Status struct {
	CheckinEnabled     bool   `json:"checkin_enabled"`
	CheckinHours       []int  `json:"checkin_hours"`
	CheckinNext        string `json:"checkin_next,omitempty"` // RFC3339
	KeepaliveEnabled   bool   `json:"keepalive_enabled"`
	KeepaliveHours     []int  `json:"keepalive_hours"`
	KeepaliveNext      string `json:"keepalive_next,omitempty"`
	BalanceIntervalSec int    `json:"balance_interval_sec,omitempty"`
}

// Status 返回当前排程（只读快照）。上次执行不在服务端留痕（日志里有），这里只报下一次。
func (s *Scheduler) Status() Status {
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Status{
		CheckinEnabled:     s.cfg.CheckinEnabled,
		CheckinHours:       s.cfg.CheckinHours,
		KeepaliveEnabled:   s.cfg.KeepaliveEnabled,
		KeepaliveHours:     s.cfg.KeepaliveHours,
		BalanceIntervalSec: int(s.cfg.BalanceRefreshInterval / time.Second),
	}
	if st.CheckinEnabled {
		if t := nextFire(now, s.cfg.CheckinHours); !t.IsZero() {
			st.CheckinNext = t.Format(time.RFC3339)
		}
	}
	if st.KeepaliveEnabled {
		if t := nextFire(now, s.cfg.KeepaliveHours); !t.IsZero() {
			st.KeepaliveNext = t.Format(time.RFC3339)
		}
	}
	return st
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			// 日历日 +1（Date 会归一 DST），不是 +24h：春季跳表那天 +24h 会把时点漂到
			// 隔壁小时（3/7 03:00 EST + 24h = 3/8 04:00 EDT），那一档就被静默跳过。
			t = time.Date(now.Year(), now.Month(), now.Day()+1, h, 0, 0, 0, now.Location())
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// shanghaiTZ 固定东八区：上游的「今天」按北京时间算，而进程可能跑在 UTC/别的时区，
// 用 time.Local 会在境外服务器上把「今天」算错一天（每日积分 00:00 发放的边界就在那儿）。
var shanghaiTZ = time.FixedZone("CST", 8*3600)

// sameDayShanghai 两个时刻是否落在同一个北京日期。
func sameDayShanghai(a, b time.Time) bool {
	return a.In(shanghaiTZ).Format("2006-01-02") == b.In(shanghaiTZ).Format("2006-01-02")
}

// Run 主循环，阻塞直到 ctx 取消；余额刷新与补签扫描各起一条循环（周期与整点无关）。
//
// 排程可以热改（ApplySchedule 关旧 wakeCh 换代 → 下面的 <-wake 立刻返回）：收到就丢掉旧计划、
// 按新排程重算下一次时点。wake 每轮都要重新取（wakeup()），否则换代后等的还是已关闭的旧 chan。
// 注意 next 从**上一个计划时点**推进而不是每轮 time.Now()：任务耗时跨过相邻两档时点时，
// 用墙上时间重算会看到「下一档已经过去」直接跳到次日，把中间那档静默吞掉。
func (s *Scheduler) Run(ctx context.Context) {
	go s.runBalanceLoop(ctx)
	go s.runCheckinRetryLoop(ctx)
	var (
		hours     []int
		next      time.Time
		replanned bool // 刚被 wake 叫醒：排完新时点打一行日志，便于确认热改真的生效
	)
	for {
		wake := s.wakeup()
		if hours == nil {
			hours = s.allHours()
		}
		if len(hours) == 0 {
			// 两个任务都关了：没有时点要等，躺到退出或排程被改成有任务为止。
			select {
			case <-ctx.Done():
				return
			case <-wake:
				hours = nil
				replanned = true
			}
			continue
		}
		if next.IsZero() {
			next = nextFire(time.Now(), hours)
			if replanned {
				s.mu.RLock()
				ci, ch := s.cfg.CheckinEnabled, append([]int(nil), s.cfg.CheckinHours...)
				ka, kh := s.cfg.KeepaliveEnabled, append([]int(nil), s.cfg.KeepaliveHours...)
				s.mu.RUnlock()
				// 分开报两个任务：hours 是两者合并后的集合，混在一起看不出哪个任务在几点。
				log.Printf("[schedule] 排程已热更新：签到=%v 时点%v，保活=%v 时点%v，下次触发 %s",
					ci, ch, ka, kh, next.Format("2006-01-02 15:04"))
				replanned = false
			}
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-wake:
			// 排程改了：时点集合与开关都可能变，重算（保留「从计划时点推进」的语义
			// 不适用这里——旧计划已作废，按当下重新排）。
			timer.Stop()
			hours = nil
			next = time.Time{}
			replanned = true
		case <-timer.C:
			fired := next
			next = nextFire(fired, hours)
			h := fired.Hour()
			s.mu.RLock()
			doCheckin := s.cfg.CheckinEnabled && contains(s.cfg.CheckinHours, h)
			doKeepalive := s.cfg.KeepaliveEnabled && contains(s.cfg.KeepaliveHours, h)
			s.mu.RUnlock()
			if doCheckin {
				s.RunCheckinNow()
			}
			if doKeepalive {
				s.RunRefreshNow()
			}
		}
	}
}

// checkinRefreshSkew 签到前预刷新窗口：access token 剩不到这么久就先换一张。
const checkinRefreshSkew = 2 * time.Hour

// checkinGap / checkinGapJitter 多账号签到之间的间隔（基准 + 0..jitter 抖动）。
// 上游按设备/时段限流，背靠背连签容易吃到 9074「当前参与用户太多」——star620/TraeTools
// 用的是同一手（账号间 3~6s 随机间隔）。三条号也就多花 ~10s，且这是小时级任务。
// 变量而非常量：测试要把它置 0，不然每条用例白等十几秒。
var checkinGap, checkinGapJitter = 3 * time.Second, 3 * time.Second

// checkinPace 两个账号之间该等多久；<=0 表示不等（测试）。
func checkinPace() time.Duration {
	if checkinGap <= 0 {
		return 0
	}
	d := checkinGap
	if checkinGapJitter > 0 {
		d += time.Duration(rand.Int64N(int64(checkinGapJitter)))
	}
	return d
}

// bumpCheckinDevice 换一套设备指纹（设备维度限流/已领时用）。
// 失败只记日志：换指纹是补救动作，它失败不该把这次签到的结论改掉。
func (s *Scheduler) bumpCheckinDevice(uid string) {
	machineID, ok, err := s.cfg.Pool.RotateMachineID(uid)
	switch {
	case !ok:
		log.Printf("checkin device bump %s: 账号不在池里", uid)
	case err != nil:
		log.Printf("checkin device bump %s: machineId=%s 落盘失败: %v", uid, machineID, err)
	default:
		log.Printf("checkin device bump %s: 已换设备指纹 machineId=%s", uid, machineID)
	}
}

// isSessionDead 上游判定的「凭证失效」（401/1001 家族）：签到路径据此换票重试一次。
func isSessionDead(err error) bool {
	var ue *upstream.Error
	return errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead
}

// checkinRetryInterval 补签扫描间隔。
//
// 上游 claim 的 9074（「当前参与用户太多」）此前误判为纯时段性拥塞；2026-10-02 实测：
// 零星设备头会被系统性拒绝（三天 ~150 次全灭），完整客户端伪装头（headers.go ug 族）
// 修复后一次通过。每小时重扫保留作波动保险。
// 变量而非常量只为测试能把它调小。
var checkinRetryInterval = time.Hour

// runCheckinRetryLoop 当天补签：每小时扫一遍，没签上的账号在下一个整点还有机会。
//
// 反复扫是安全的：CheckinUID 先查 status，已签的账号直接跳过（幂等），签到域冷却中的账号
// 也会被 byUrgency 跳过——上游拥塞时不会退化成连打。
// 「进程起来先扫一遍」不在这里做：`catchUpCheckin` 已经负责启动补跑，重复调用会对上游连打两次。
// ponytail: 固定 1 小时。要更密/更疏就把它做成配置项。
func (s *Scheduler) runCheckinRetryLoop(ctx context.Context) {
	// 签到这个任务被关掉时也留在这条循环里等（wake 会把它叫醒）：否则面板打开签到后
	// 补签扫描要等到重启才出现。
	for {
		wake := s.wakeup()
		s.mu.RLock()
		on := s.cfg.CheckinEnabled
		s.mu.RUnlock()
		if !on {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			}
			continue
		}
		t := time.NewTimer(checkinRetryInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-wake:
			t.Stop()
		case <-t.C:
			s.RunCheckinNow()
		}
	}
}

// runBalanceLoop 周期刷新余额；周期是热项，每轮重读（<=0 = 暂停，等 wake 叫醒）。
func (s *Scheduler) runBalanceLoop(ctx context.Context) {
	for {
		wake := s.wakeup()
		s.mu.RLock()
		interval := s.cfg.BalanceRefreshInterval
		s.mu.RUnlock()
		if interval <= 0 {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			}
			continue
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-wake:
			t.Stop()
		case <-t.C:
			s.RunBalanceRefresh()
		}
	}
}

// entUsageOf 拉一次额度并写进池子，返回**口径感知**的快照。
//
// 用 UserEntUsageDetail 而不是老接口 UserEntUsage：国际版是**美元**计费、积分恒 0
// （2026-10-15 实测 is_dollar_usage_billing=true、basic_usage_limit=1、basic_usage_amount=0.00059），
// 老接口把国际号的额度一律读成 0，面板就显示成「无额度」——那不是上游的真实状态。
func (s *Scheduler) entUsageOf(uid string, a *auth.Auth) (upstream.EntUsage, error) {
	// 用带到期的口径：面板「到期」列与「积分到期提醒」都读 pool 里的 expire，
	// 而余额接口本身不返回到期时间（见 upstream.UserEntUsageDetailWithExpiry 的说明）。
	u, err := s.cfg.Upstream.UserEntUsageDetailWithExpiry(a)
	if err != nil {
		return u, err
	}
	s.cfg.Pool.SetBalance(uid, pool.Balance{
		Unit: u.Unit, Remain: u.Remain, Total: u.Total,
		UsedUSD: u.UsedUSD, LimitUSD: u.LimitUSD,
	}, u.Expire)
	s.cfg.Pool.SetExpireCredits(uid, u.ExpirePoints)
	return u, nil
}

// RunBalanceRefresh 刷新全部账号的积分与到期时间；返回查询失败的账号数。
//
// 与签到的区别：只读余额，不刷 token、**不解冻**。"余额 > 0 就解冻"是签到那一步的语义
// （用户手动签到就是为了解冻），套到周期刷新上会让熔断/降权/计划冷却每 5 分钟被清一次，
// 等于这些机制不存在。
func (s *Scheduler) RunBalanceRefresh() int {
	failed := 0
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		if _, err := s.entUsageOf(st.UID, a); err != nil {
			failed++
			continue
		}
	}
	if failed > 0 {
		log.Printf("balance refresh: %d 个账号查询失败", failed)
	}
	return failed
}

func contains(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}

// RunCheckinNow 立即对所有账号执行签到 + 积分刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
//
// 返回逐账号结果（面板据此回报「本次 +N 分 / 已签 / 失败原因」）与失败列表（日志与 HTTP 状态码用）。
func (s *Scheduler) RunCheckinNow() ([]CheckinResult, []error) {
	// 单飞：整点、补签 ticker、面板按钮、启动补跑四路并发时只放一轮进来。
	if !s.checkinLock.TryLock() {
		log.Printf("[checkin] 上一轮还在跑，跳过这一轮")
		return nil, nil
	}
	defer s.checkinLock.Unlock()
	results := []CheckinResult{} // 非 nil：面板拿到的是 [] 而不是 null
	var errs []error
	for i, uid := range s.byUrgency() {
		if i > 0 {
			time.Sleep(checkinPace()) // 第一个不等；最后一个签完也不用再等
		}
		res, err := s.CheckinUID(uid)
		results = append(results, res)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", uid, err))
		}
	}
	return results, errs
}

// byUrgency 探一轮余额，按最早到期时间升序返回待签 uid；无到期信息的排最后。
// 探失败不剔除账号（仍要去签，失败原因由 CheckinUID 报出来），只是没有紧迫度信息。
// ponytail: 每账号多一次只读探询换准确排序；号多了改成复用上一轮落盘的 expire。
func (s *Scheduler) byUrgency() []string {
	type row struct {
		uid    string
		expire int64
	}
	rows := make([]row, 0)
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		// 签到域冷却中的账号跳过本轮：上游拥塞时反复重试只会更堵。
		if s.cfg.Pool.CheckinCooling(st.UID) {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		// 签到只依赖 access_token（v1.1.0 凭证可能没有 refresh_token——CheckinUID
		// 已放开前置否决，这里同步别再按票筛掉，否则自动签到永远排不进该号）。
		// 所有号都要进签到队列：小浣熊的每日积分发放对所有账号一致（
		// CheckinUID 会按「上游未开」如实跳过，而不是这里直接不排队）。
		expire := int64(0)
		if u, err := s.entUsageOf(st.UID, a); err == nil {
			expire = u.Expire
		}
		rows = append(rows, row{uid: st.UID, expire: expire})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].expire, rows[j].expire
		if a == 0 {
			return false // 无到期信息排最后
		}
		if b == 0 {
			return true
		}
		return a < b
	})
	uids := make([]string, 0, len(rows))
	for _, r := range rows {
		uids = append(uids, r.uid)
	}
	return uids
}

// CheckinResult 单个账号的签到结果，给面板如实回报用：
// 用户签完得知道「本次拿了多少分」，而不是只看到一句「签到完成」。
type CheckinResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Status   string `json:"status"`            // ok | already | skipped（上游未开）| failed
	Credits  int64  `json:"credits,omitempty"` // 本次到账（仅 ok 非 0，取上游 status 告知的可领额度）
	Reason   string `json:"reason,omitempty"`  // 失败原因原文
}

// CheckinUID 签到并按剩余积分解冻单个账号。禁用返回 ErrDisabled。
// 返回的 CheckinResult 说明这次到底发生了什么（含本次拿到的积分），错误路径同样带结果。
func (s *Scheduler) CheckinUID(uid string) (CheckinResult, error) {
	res := CheckinResult{UID: uid}
	st, ok := s.cfg.Pool.Status(uid)
	if !ok {
		return res, ErrNotFound
	}
	res.Nickname = st.Nickname
	if st.Disabled {
		return res, ErrDisabled
	}
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return res, ErrNotFound
	}
	// 没有 refresh_token 不再一票否决：v1.1.0 的 box-agent 凭证只有 access_token
	// （实测 auth.json 不再落 refresh），签到/查询本身用 access_token 就够。
	// 预刷新逻辑自己会跳过未到期票；真到期又无票时，RefreshTokenIfNeeded 报错、
	// 后面 status 调用如实暴露「请重新登录」，不在这里提前拦。
	// 每日发放是服务端 00:00 自动入账（bills 流水实锤），CheckinStatus 以流水里
	// 当天有无 daily_grant 为准；enable=false 只在「上游把该账号额度关了」时出现。
	// 签到前把 access token 换新的：9 点签到离上一次保活（凌晨 3 点）有段距离，拿一张
	// 已过期的票只会白跑一轮（照 wild-work：2h 内到期就先刷）。刷失败不在这里报错——真有
	// 问题会让下面的 status 调用如实暴露，那条错误信息比「刷新失败」更贴题。
	// 但**真刷了就得落盘**：上游此时已轮换 refreshToken，盘上留旧票的话，进程在下次成功
	// 刷新前重启 → 该号被判 session 失效并自动禁用（要人工重登）。
	if refreshed, err := s.cfg.Upstream.RefreshTokenIfNeeded(a, checkinRefreshSkew); err != nil {
		log.Printf("checkin refresh %s: %v", uid, err)
	} else if refreshed {
		if serr := a.SaveAtomic(); serr != nil {
			log.Printf("checkin refresh %s: 新票落盘失败: %v", uid, serr)
		}
	}
	checkedIn, reward, enable, serr := s.cfg.Upstream.CheckinStatus(a)
	// token 被别处轮换/提前吊销 → status 报 session 失效：换一张新票再查一次，别把
	// 「票过期」记成签到失败（这条只重试一次，重试前必须真的换到了新票）。
	if isSessionDead(serr) {
		if rerr := s.cfg.Upstream.RefreshToken(a); rerr != nil {
			log.Printf("checkin token refresh %s: %v", uid, rerr)
		} else {
			// RefreshToken 一定轮换了票，同样必须落盘（理由同上）。
			if serr := a.SaveAtomic(); serr != nil {
				log.Printf("checkin token refresh %s: 新票落盘失败: %v", uid, serr)
			}
			if checkedIn, reward, enable, serr = s.cfg.Upstream.CheckinStatus(a); serr == nil {
				log.Printf("checkin status %s: status ok after token refresh", uid)
			}
		}
	}
	var checkinErr error
	switch {
	case serr != nil:
		checkinErr = serr
		log.Printf("checkin status %s: %v", uid, serr)
	case checkedIn:
		res.Status = "already"
		log.Printf("checkin %s: already checked in", uid)
	case !enable:
		// 上游把签到关了这个账号（enable=false）：什么都没做，如实报出来。
		res.Status = "skipped"
		log.Printf("checkin %s: upstream disabled (enable=false)", uid)
	default:
		claimErr := s.claimWithRetry(a)
		switch {
		case claimErr == nil:
			// 交给下面的复核判定
		case errors.Is(claimErr, upstream.ErrCheckinAlready):
			// 9095「今日已签到」**不能**当「本账号已签」：上游这条信息是**设备维度**的，
			// 同一台设备可能已经替另一个账号签过（Trae2api-cn 原注释：只有 status 能定论）。
			// 不换设备的话下一轮还会回 9095——所以顺手换一套指纹，再由复核给结论。
			s.bumpCheckinDevice(uid)
			log.Printf("checkin %s: claim 9095（设备维度已签）→ 换设备指纹，以 status 复核", uid)
		default:
			checkinErr = claimErr
			log.Printf("checkin claim %s: %v", uid, claimErr)
			// 9074「当前参与用户太多」同样是设备维度限流：换一套指纹让**下一轮**脱开旧标记。
			// 这里刻意不立刻重试——上游原注释说秒级内第二次 claim 会延长限流窗口
			// （Trae2api-cn trae_client.py），补签扫描/手动点击自然带新身份再试。
			if upstream.IsCheckinRateLimited(claimErr) {
				s.bumpCheckinDevice(uid)
			}
		}
		// 复核（claim 成功与 9095 两条路径都要走）：只有 status 说 checked_in 才算签上。
		// claim 回 code 0 不等于真签上（上游偶发只回执不落账）；9095 更是设备维度信息。
		if checkinErr == nil {
			if verified, _, _, verr := s.cfg.Upstream.CheckinStatus(a); verr != nil {
				checkinErr = fmt.Errorf("签到复核失败：%w", verr)
				log.Printf("checkin verify %s: %v", uid, verr)
			} else if !verified {
				// 当天新注册的号：每日积分发放是上游 00:00 的定时任务，今天这批根本还没发，
				// bills 里不会有 daily_grant。这不是签到失败（claim 该拿的登录奖励已拿到），
				// 报 failed 还会给它记一次 5 分钟签到冷却、把面板刷成一脸错。
				if created, ok := s.cfg.Upstream.UserCreatedAt(a); ok && sameDayShanghai(created, time.Now()) {
					res.Status = "pending"
					res.Reason = "上游今日尚未发放每日积分（" + created.In(shanghaiTZ).Format("2006-01-02 15:04") +
						" 注册的新号，首批在次日 00:00 发放）"
					log.Printf("checkin %s: pending（%s 注册的新号，等次日 00:00 首批每日积分）",
						uid, created.In(shanghaiTZ).Format("2006-01-02 15:04"))
				} else {
					checkinErr = &upstream.BusinessError{Message: "签到复核未通过：上游仍报未签到"}
					log.Printf("checkin verify %s: status still checked_in=false", uid)
				}
			} else {
				res.Status, res.Credits = "ok", reward
				log.Printf("checkin %s: ok +%d", uid, reward)
			}
		}
	}
	if res.Status == "" {
		res.Status = "failed"
	}
	if checkinErr != nil {
		res.Reason = checkinErr.Error()
	}
	u, err := s.entUsageOf(uid, a)
	if err != nil {
		log.Printf("ent-usage %s: %v", uid, err)
		return res, err
	}
	// 先落余额与到期时间（签到后的新值），再谈解冻/冷却。
	// 「签到解冻」（ReenableIfCredits 在 remain>0 时清网关冷却）**只在成功路径上做**：
	// 失败路径也调它，等于一次失败签到顺手把 12h 计划冷却冲掉。它不碰签到域冷却，
	// 所以下面那条路径不会把签到冷却冲掉。
	//
	// 这里传积分口径的 u.Remain：签到只存在于国内路径（国际版在本函数开头就 return 了），
	// 所以拿到的必然是积分口径，美元号的 0 不会走进来。
	if checkinErr == nil {
		s.cfg.Pool.ReenableIfCredits(uid, u.Remain)
		s.cfg.Pool.ClearCheckinCooldown(uid)
		return res, nil
	}
	// 签到失败：记一次签到域冷却，避免下一轮对拥塞的上游反复重试
	// （9074「当前参与用户太多」是上游拥塞，重试只会更堵）。
	// 只挡签到轮次，不进网关选号——对话额度跟签到接口拥塞无关。
	if d, ok := checkinCooldown(checkinErr); ok {
		s.cfg.Pool.CooldownCheckin(uid, d, checkinErr.Error())
	}
	return res, checkinErr
}

// checkinCooldown 按签到失败类型决定冷却时长。
// 业务失败（HTTP 200 + 非 0 业务码）不会因重试而改变，短冷却只为不再连打上游；
// plan 权益不足要等很久，给长冷却。
func checkinCooldown(err error) (time.Duration, bool) {
	var be *upstream.BusinessError
	if errors.As(err, &be) {
		if be.Code == 1005 {
			return 12 * time.Hour, true
		}
		return 5 * time.Minute, true
	}
	var ue *upstream.Error
	if errors.As(err, &ue) && ue.Kind == upstream.ErrSoftRate {
		return time.Minute, true
	}
	return 0, false
}

// claimWithRetry 只在网络层失败时重试一次。
// 业务失败与 HTTP 错误不会因重试而改变，重试只会成倍增加上游请求、触发风控。
func (s *Scheduler) claimWithRetry(a *auth.Auth) error {
	err := s.cfg.Upstream.CheckinClaim(a)
	if err == nil {
		s.claimMobileGrant(a)
		return nil
	}
	if !isNetworkError(err) {
		return err
	}
	time.Sleep(time.Second)
	if err := s.cfg.Upstream.CheckinClaim(a); err != nil {
		return err
	}
	s.claimMobileGrant(a)
	return nil
}

// claimMobileGrant 顺手领手机端首登奖励：一次性福利，与桌面端那笔额度独立。
//
// 失败只记日志：一笔一次性奖励不该让整轮签到判失败（它跟每日积分无关）。
// 结算状态只在进程内记（见 auth.Auth.mobileGranted），重启后多打一次上游，
// 幂等接口回 granted:false 即可。
func (s *Scheduler) claimMobileGrant(a *auth.Auth) {
	if a.MobileGrantDone() {
		return
	}
	granted, err := s.cfg.Upstream.ClaimMobileGrant(a)
	if err != nil {
		log.Printf("mobile-grant %s: %v", a.UID, err)
		return
	}
	a.MarkMobileGrantDone()
	log.Printf("mobile-grant %s: granted=%v", a.UID, granted)
}

// isNetworkError 判断是否传输层失败：http.Client 会把传输错误包成 *url.Error，
// 而 HTTP 4xx/5xx 与业务失败走的是 *upstream.Error / *upstream.BusinessError。
func isNetworkError(err error) bool {
	var uerr *url.Error
	return errors.As(err, &uerr)
}

// CatchUp 启动补跑：已过当天最早的签到时点就补签一次。
// 排程定在 9 点而进程 10 点才起来是常态，不补跑等于当天整天不签。
//
// 不记台账：CheckinUID 先探上游状态，已签到不会重复 claim，
// 代价是进程当天反复重启时会多几次只读探询。
func (s *Scheduler) CatchUp() {
	s.mu.RLock()
	enabled := s.cfg.CheckinEnabled
	hours := append([]int(nil), s.cfg.CheckinHours...)
	s.mu.RUnlock()
	if !enabled || len(hours) == 0 {
		return
	}
	earliest := slices.Min(hours)
	if h := time.Now().Hour(); h < earliest {
		return
	}
	log.Printf("checkin catch-up: 已过 %d 点签到小时，补跑一轮", earliest)
	if _, errs := s.RunCheckinNow(); len(errs) != 0 {
		log.Printf("checkin catch-up: %d 个账号失败", len(errs))
	}
}

// RunRefreshNow 立即对所有账号刷新 token（保活：不看剩余有效期，每天无条件刷一遍）。
//
// 为什么不按 RefreshSkew 只刷快过期的：access token 活 14 天，按「过期前 24h」刷等于
// refresh token 链十天半个月没人碰；上游一旦让它过期就只能重登（掉线）。
// session 失效不立刻杀号：连续 SessionDeadThreshold 次才禁用，成功即清零。
func (s *Scheduler) RunRefreshNow() {
	// 单飞：与签到同理，保活不该被两路同时跑（同一账号两次 ExchangeToken 会互相轮换掉票）。
	if !s.refreshLock.TryLock() {
		log.Printf("[keepalive] 上一轮还在跑，跳过这一轮")
		return
	}
	defer s.refreshLock.Unlock()
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("refresh %s: %v", st.UID, err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("refresh %s: 连续 %d 次 session 失效 — 禁用", st.UID, pool.SessionDeadThreshold())
				}
			}
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID)
		if err := a.SaveAtomic(); err != nil {
			log.Printf("refresh %s save: %v", st.UID, err)
		}
	}
}
