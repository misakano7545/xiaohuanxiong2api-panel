// Package panel 同进程管理页：账号池、模型、配置、日志。
package panel

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/pool"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/reqlog"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/scheduler"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/upstream"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/usage"
)

// Config 面板依赖。AuthDir 用进程启动时的目录，改配置里的路径要重启才生效。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	Scheduler *scheduler.Scheduler
	AuthDir   string
	APIKey    string
	Version   string
	// BindAll listen 绑了所有网卡（空密钥时面板对全网开放，不只是本机）——app.js 的提示要分开说。
	BindAll bool
	// CallbackBase 环回授权回调的基址（http://127.0.0.1:<端口>）。空 = 不支持浏览器授权，
	// 面板只能走「导入账号」粘贴。官方授权页只认 127.0.0.1 的环回地址，所以这里不能用 LAN IP。
	CallbackBase string
	Logs         *Ring
	Models       func() []map[string]any
	Usage        *usage.Recorder // 可选，用量台账
	// RequestLog 可选，请求级指标/归档（运行日志页的「请求记录」）。nil = 该接口 501。
	RequestLog *reqlog.Recorder
	ConfigPath string
	LoadConfig func() (any, error)
	SaveConfig func(raw []byte) ([]string, error)
}

// Panel 挂在 /panel/ 下，pattern 使用完整路径。
type Panel struct {
	cfg     Config
	mux     *http.ServeMux
	started time.Time
	logs    *Ring

	// apiKey 随配置热改（面板保存配置后 ApplyAPIKey）。独立存一份而不是读 cfg.APIKey：
	// cfg 是 New 时复制进来的快照，配置改了它不会变。
	keyMu  sync.RWMutex
	apiKey string

	loginMu sync.Mutex
	logins  map[string]loginSession

	// sms 短信下发频控（见 sms.go）：自架面板不能被人当短信轰炸器用。
	sms smsGuard
}

// New 构建面板。Logs 为空时自建一个环，但 main 应传入和标准日志共用的那个。
func New(cfg Config) *Panel {
	if cfg.Logs == nil {
		cfg.Logs = NewRing(400)
	}
	p := &Panel{
		cfg:     cfg,
		apiKey:  cfg.APIKey,
		mux:     http.NewServeMux(),
		started: time.Now(),
		logs:    cfg.Logs,
		logins:  map[string]loginSession{},
	}
	p.mux.HandleFunc("GET /panel/{$}", p.index)
	p.mux.HandleFunc("GET /panel/app.js", p.appScript)
	p.mux.HandleFunc("GET /panel/api/overview", p.withAuth(p.overview))
	p.mux.HandleFunc("GET /panel/api/logs", p.withAuth(p.logsHandler))
	p.mux.HandleFunc("GET /panel/api/request_logs", p.withAuth(p.requestLogs))
	p.mux.HandleFunc("GET /panel/api/models", p.withAuth(p.models))
	p.mux.HandleFunc("GET /panel/api/packages", p.withAuth(p.packages))
	p.mux.HandleFunc("GET /panel/api/usage", p.withAuth(p.usage))
	p.mux.HandleFunc("POST /panel/api/usage/save", p.withAuth(p.usageSave))
	p.mux.HandleFunc("GET /panel/api/config", p.withAuth(p.getConfig))
	p.mux.HandleFunc("POST /panel/api/config", p.withAuth(p.saveConfig))
	p.mux.HandleFunc("POST /panel/api/login/start", p.withAuth(p.loginStart))
	// 环回授权回调不带 Bearer：它是浏览器跳转过来的，不是面板前端发的请求（见 authCallback）。
	p.mux.HandleFunc("GET /auth/callback", p.authCallback)
	p.mux.HandleFunc("GET /panel/api/login/poll", p.withAuth(p.loginPoll))
	// 微信扫码登录：不需要回调地址，服务器/局域网部署也能用（见 qrlogin.go）。
	p.mux.HandleFunc("POST /panel/api/login/qr/start", p.withAuth(p.loginQrStart))
	p.mux.HandleFunc("GET /panel/api/login/qr.png", p.withAuth(p.loginQrImage))
	// 短信登录的人机校验辅助页（独立文档 + 自己的 CSP，见 smscaptcha.go）。
	// 不带 Bearer：这是 window.open 打开的页面，且它不含任何面板数据、不调面板接口。
	p.mux.HandleFunc("GET /panel/sms-captcha", p.smsCaptchaPage)
	// 短信验证码登录（需要过一次阿里云滑块，见 sms.go）。
	p.mux.HandleFunc("POST /panel/api/login/sms/send", p.withAuth(p.smsSend))
	p.mux.HandleFunc("POST /panel/api/login/sms/verify", p.withAuth(p.smsVerify))
	// 粘贴回调链接（跨机/服务器部署时 127.0.0.1 回调到不了，走这条）。
	p.mux.HandleFunc("POST /panel/api/login/code", p.withAuth(p.loginCode))
	// refreshToken 直登：不经过浏览器把号加进来（对方只给了一条 refresh_token 时的备用入口）。
	p.mux.HandleFunc("POST /panel/api/login/refresh", p.withAuth(p.loginRefresh))
	p.mux.HandleFunc("POST /panel/api/checkin", p.withAuth(p.checkinAll))
	p.mux.HandleFunc("POST /panel/api/balance", p.withAuth(p.balanceAll))
	p.mux.HandleFunc("POST /panel/api/keepalive", p.withAuth(p.keepaliveAll))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/checkin", p.withAuth(p.accountCheckin))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/balance", p.withAuth(p.accountBalance))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/disable", p.withAuth(p.accountDisable))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/enable", p.withAuth(p.accountEnable))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/clear-cooldown", p.withAuth(p.accountClear))
	// 换 ug 族设备指纹（手动解 9074 风控标记）。
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/rotate-device", p.withAuth(p.accountRotateDevice))
	p.mux.HandleFunc("POST /panel/api/accounts/rotate-device-all", p.withAuth(p.rotateDeviceAll))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/remove", p.withAuth(p.accountRemove))
	// 运维单号动作与批量导入（见 ops.go）。导入端点收「回调链接」或「凭证 JSON」裸文本。
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/refresh", p.withAuth(p.accountRefreshToken))
	p.mux.HandleFunc("GET /panel/api/accounts/{uid}/json", p.withAuth(p.accountCredential))
	p.mux.HandleFunc("POST /panel/api/accounts/import", p.withAuth(p.accountImport))
	p.mux.HandleFunc("GET /panel/api/checkin_status", p.withAuth(p.checkinStatus))
	return p
}

func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	p.mux.ServeHTTP(w, r)
}

func (p *Panel) authorized(r *http.Request) bool {
	want := p.key()
	if want == "" {
		// 空密钥 = 只信本机。写操作再收一道同源：本机浏览器上的任意网页都能发跨站简单
		// 请求（表单/fetch），RemoteAddr 依旧是回环，只看 IP 等于对它们全放行——实测
		// 跨站 POST .../remove 会真把凭证删掉。读操作保持原样。
		if !isLoopback(r) {
			return false
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			return true
		}
		return sameOrigin(r)
	}
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(want)) == 1
}

// key 当前生效的面板密钥（空 = 不鉴权）。
func (p *Panel) key() string {
	p.keyMu.RLock()
	defer p.keyMu.RUnlock()
	return p.apiKey
}

// ApplyAPIKey 换面板密钥，立即生效（面板保存配置改了 api_key 时调用）。
func (p *Panel) ApplyAPIKey(k string) {
	p.keyMu.Lock()
	p.apiKey = k
	p.keyMu.Unlock()
}

// sameOrigin 写请求是不是面板自己发的：浏览器跨站请求一定带自己的 Origin/Referer，
// 落不到本机或面板自己的 host 上就拒。非浏览器客户端（curl）不带这两个头，放行。
func sameOrigin(r *http.Request) bool {
	ref := r.Header.Get("Origin")
	if ref == "" {
		ref = r.Header.Get("Referer")
	}
	if ref == "" {
		return true
	}
	u, err := url.Parse(ref)
	if err != nil {
		return false
	}
	h := u.Hostname()
	// r.Host 可能带端口、也可能是反代域名：只比主机名部分。
	if hh, _, err := net.SplitHostPort(r.Host); err == nil {
		return strings.EqualFold(h, hh)
	}
	return strings.EqualFold(h, r.Host)
}

// isLoopback 请求是否来自本机。见过转发头就直接判否：隧道/反代（Cloudflare、nps、frp）
// 会把 RemoteAddr 变成 127.0.0.1 并补上转发头，那时「空 key = 仅本机」等于对公众全放行。
func isLoopback(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-Ip") != "" || r.Header.Get("Forwarded") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (p *Panel) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 面板 API 一律不缓存：/panel/api/config 的响应体里是明文密钥，/overview 里有账号状态。
		w.Header().Set("Cache-Control", "no-store")
		if !p.authorized(r) {
			writeErr(w, http.StatusUnauthorized, "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

// usagePanelHours 账号池「用量」列的统计窗口，与用量视图默认窗口一致。
const usagePanelHours = 72

// accountRow 账号状态 + 窗口内的用量汇总。嵌入后 JSON 平铺，前端仍按 uid/credits/... 读账号字段，
// 用量在 a.usage（无记录时为 null）。
type accountRow struct {
	pool.Status
	Usage *usage.Agg `json:"usage,omitempty"`
	// 累计口径（不受用量视图窗口影响），字段名与 wb 对齐：
	// 池级重试放大/坏号靠「成功 / 失败 + 在途（Status.InFlight）」一眼可见。
	SuccessCount int64     `json:"success_count,omitempty"`
	ErrTotal     int64     `json:"err_total,omitempty"`
	LastSuccess  time.Time `json:"last_success,omitempty"`
	// ug 族设备指纹（签到/积分用的那套，按 uid + 种子派生）：面板展示 + 「换指纹」用。
	// 9074 是设备维度风控，换一个设备号就能脱开旧标记（star620/TraeTools 的做法）。
	MachineID string `json:"machine_id,omitempty"`
	// TokenExpiresAt access token 到期时刻（Unix 秒）。与 Status.Expire（积分包到期）不是
	// 一回事：那个是「积分什么时候作废」，这个是「这张票还能用多久」。
	TokenExpiresAt int64 `json:"token_expires_at,omitempty"`
}

func (p *Panel) overview(w http.ResponseWriter, r *http.Request) { p.writeOverview(w, nil) }

// writeOverview 面板首屏 JSON。extra 让动作类端点（签到）把「本次发生了什么」搭车带回：
// 前端一次请求就拿到账号状态 + 结果，不必再猜本次拿了多少分。
func (p *Panel) writeOverview(w http.ResponseWriter, extra map[string]any) {
	list := p.cfg.Pool.List()
	var healthy, cooling, disabled int
	for _, st := range list {
		switch {
		case st.Disabled:
			disabled++
		case st.Cooling:
			cooling++
		default:
			healthy++
		}
	}
	// 用量列与用量视图同源（同一份台账、同一窗口）：另存一份每账号累计计数器早晚会和台账
	// 对不上，届时运维得先分辨该信哪个。台账为空/无调用时 usage 缺席，前端显示「—」。
	rows := make([]accountRow, len(list))
	for i, st := range list {
		rows[i] = accountRow{Status: st}
		if a := p.cfg.Pool.AuthByUID(st.UID); a != nil {
			rows[i].MachineID = a.MachineIDValue()
			rows[i].TokenExpiresAt = a.ExpiresAtValue()
		}
	}
	for _, a := range p.cfg.Usage.Snapshot(usagePanelHours, p.nicks()).ByAccount {
		for i := range rows {
			if rows[i].UID != a.Key {
				continue
			}
			agg := a.Agg
			rows[i].Usage = &agg
			// 成功数 = 全历史请求 - 全历史失败：台账每次尝试都记一次，成败都由那一个
			// ok 标记决定，不另算一份计数器。
			if a.Life != nil {
				rows[i].SuccessCount = a.Life.Requests - a.Life.Errors
				rows[i].ErrTotal = a.Life.Errors
			}
			rows[i].LastSuccess = a.LastOK
			break
		}
	}
	// agent 基址现状：上游换域名时这里是第一现场（自动发现 > 显式配置 > 内置常量）。
	hostInfo := map[string]any{}
	if p.cfg.Upstream != nil {
		global, cn := p.cfg.Upstream.AgentHosts()
		hostInfo["agent_host_intl"] = global
		hostInfo["agent_host_cn"] = cn
	}
	// key=有密钥；loopback=空密钥且只绑本机；open=空密钥但绑了所有网卡（面板"本机免鉴权"
	// 的说明此时是错的，前端按这个字段分开提示）。
	scope := "key"
	if p.key() == "" {
		scope = "loopback"
		if p.cfg.BindAll {
			scope = "open"
		}
	}
	body := map[string]any{
		"version":       p.cfg.Version,
		"uptime_sec":    int(time.Since(p.started).Seconds()),
		"auth_required": p.key() != "",
		"auth_scope":    scope,
		"total":         len(list),
		"healthy":       healthy,
		"cooling":       cooling,
		"disabled":      disabled,
		"usage_hours":   usagePanelHours,
		"accounts":      rows,
	}
	for k, v := range hostInfo {
		body[k] = v
	}
	// 排程现状（下次签到/保活几点）：面板首屏就能看到，不必翻日志猜。
	if p.cfg.Scheduler != nil {
		body["schedule"] = p.cfg.Scheduler.Status()
	}
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, http.StatusOK, body)
}

// nicks 池内 uid→昵称，仅供展示（不含任何凭证）。
func (p *Panel) nicks() map[string]string {
	nicks := map[string]string{}
	for _, st := range p.cfg.Pool.List() {
		if st.Nickname != "" {
			nicks[st.UID] = st.Nickname
		}
	}
	return nicks
}

func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": p.logs.Snapshot()})
}

func (p *Panel) models(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Models == nil {
		writeErr(w, http.StatusNotImplemented, "models unavailable")
		return
	}
	// 顺手按官方目录刷一次（10 分钟 TTL，失败静默回落到缓存/静态兜底）：
	// 面板这一页是「官方此刻给了什么」的展示口，不应该只有重启才更新。
	if a := p.cfg.Pool.Peek(); a != nil && p.cfg.Upstream != nil {
		if _, err := p.cfg.Upstream.RefreshModels(a, false); err != nil {
			log.Printf("panel: 模型目录刷新失败（用缓存/兜底）: %v", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": p.cfg.Models()})
}

// packages 返回全部账号的积分包构成，供「积分构成」视图对比。
// 逐账号**实时**查上游（同 balanceAll 的口径），失败的单列出来不拖累其余账号——
// 一个号凭证过期不该让整页空白。
//
// 口径感知：国内返回积分包（remain/size 是积分），国际返回美元包（remain_usd/limit_usd）。
// 实测国际 Free plan 就是一条 basic_usage_limit=1（$1）、已用 $0.00059 的包，
// 老代码只认 credits_limit 会把它整条跳过 → 面板显示「无积分套餐」，那是假象。
func (p *Panel) packages(w http.ResponseWriter, r *http.Request) {
	type item struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname,omitempty"`
		Realm    string `json:"realm,omitempty"`
		// CreditUnit 账号级口径，键名与 overview（pool.Status）**必须一致**。
		// 曾经这里叫 unit、overview 叫 credit_unit，前端 isUSD() 只认后者 →
		// 构成视图里美元号被判成积分号，整张卡显示 0（= 用户看到的那个 0）。
		// 同概念两个键名就是这类静默失效的来源，所以这里有 TestPackagesUnitKeyMatchesOverview 兜着。
		CreditUnit string                   `json:"credit_unit"`
		Remain     int64                    `json:"remain"`
		Size       int64                    `json:"size"`
		UsedUSD    float64                  `json:"used_usd,omitempty"`
		LimitUSD   float64                  `json:"limit_usd,omitempty"`
		Packages   []upstream.CreditPackage `json:"packages"`
		Error      string                   `json:"error,omitempty"`
	}
	list := p.cfg.Pool.List()
	out := make([]item, 0, len(list))
	for _, st := range list {
		it := item{UID: st.UID, Nickname: st.Nickname, Realm: st.Realm,
			CreditUnit: upstream.UnitCredit, Packages: []upstream.CreditPackage{}}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			it.Error = "凭证不可用"
			out = append(out, it)
			continue
		}
		packs, u, err := p.cfg.Upstream.BalancePackages(a)
		if err != nil {
			it.Error = err.Error()
			out = append(out, it)
			continue
		}
		it.Packages = packs
		it.CreditUnit = u.Unit
		it.Remain, it.Size = u.Remain, u.Total
		it.UsedUSD, it.LimitUSD = u.UsedUSD, u.LimitUSD
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// reqIDMarker 请求记录里请求 id 的前缀（reqlog.NewRequestID 生成，16 位 hex）。
const reqIDMarker = "req-"

// reconcileCredits 用上游扣费流水回填请求记录的积分。
//
// 小浣熊不按次回报扣了多少分（token_usage 帧里只有 token），但每笔扣费都会落一条
// 积分流水：biz_type=desktop、points<0、event_name 里带着我们出站时的 X-RACCOON-Title
// （「任务消耗 · req-xxxxxxxxxxxxxxxx」）。所以对账是 1:1 精确匹配，不是估算：
// 从 event_name 取出 rid → 找对应的请求记录 → 写入真实分值。
//
// 只在面板打开「请求记录」时跑一次（每账号一次只读流水请求），不做后台轮询：
// 流水本身滞后 1~3 分钟，用户看的时候对一次就够了。
func (p *Panel) reconcileCredits() int {
	if p.cfg.RequestLog == nil || p.cfg.Upstream == nil || p.cfg.Pool == nil {
		return 0
	}
	filled := 0
	for _, st := range p.cfg.Pool.List() {
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		rows, err := p.cfg.Upstream.RecentBills(a, 100)
		if err != nil {
			continue // 单个号查不到流水不拖累其它号，也不报错刷屏
		}
		for _, row := range rows {
			if row.Points >= 0 {
				continue // 只对消费行；发放行与我们无关
			}
			rid := requestIDFromBillEvent(row.EventName)
			if rid == "" {
				continue
			}
			if p.cfg.RequestLog.SetCredit(rid, float64(-row.Points)) {
				filled++
			}
		}
	}
	if filled > 0 {
		log.Printf("panel: 积分对账回填 %d 条请求记录", filled)
	}
	return filled
}

// requestIDFromBillEvent 从流水 event_name 里取我们自己的请求 id（req-…）。
//
// 上游把标题原样拼在「任务消耗 · <title>」后面；没带标题的老行（「桌面端 LLM 对话消耗」）
// 取不到 id，返回空串 = 这条对不上，保持面板上的「—」。
func requestIDFromBillEvent(name string) string {
	i := strings.Index(name, reqIDMarker)
	if i < 0 {
		return ""
	}
	rest := name[i+len(reqIDMarker):]
	// rid 是 req- 加 16 位 hex：只吃连续的十六进制字符，避免把后续标点带进来。
	end := 0
	for end < len(rest) {
		c := rest[end]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			end++
			continue
		}
		break
	}
	if end == 0 {
		return ""
	}
	return reqIDMarker + rest[:end]
}

// requestLogs 请求记录取数：优先读 JSONL 归档（可跨重启）；归档未开时回落到进程内最近 100 条
// metrics.recent——否则「归档默认关」会让这一页永远空白，看起来像功能没做。
// 筛选（关键字/结果）在前端做（一页最多 1000 条，零延迟）。
//
// 读之前先对一次账：上游扣费流水滞后 1~3 分钟，用户打开这一页通常就是想看「刚才那次扣了多少」，
// 顺手回填比让用户等后台轮询更直接（每账号一次只读流水请求）。
func (p *Panel) requestLogs(w http.ResponseWriter, r *http.Request) {
	p.reconcileCredits()
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	// 筛选在服务端做（不是前端筛已拉取的条目）：时间区间落在更早的时段时，
	// 「最近 N 条」里根本没有那些记录，只能让归档按区间取。判据与归档读盘共用
	// reqlog.Filter，开/关归档两条路径口径一致。
	q := r.URL.Query()
	flt := reqlog.Filter{
		Outcome:   q.Get("outcome"),
		Account:   q.Get("account"),
		Model:     q.Get("model"),
		ClientIP:  q.Get("client_ip"),
		UserAgent: q.Get("user_agent"),
		From:      parseTimeParam(q.Get("from")),
		To:        parseTimeParam(q.Get("to")),
	}
	snap := p.cfg.RequestLog.Snapshot()
	entries, err := p.cfg.RequestLog.ReadArchive(limit, flt)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// archived 报的是「归档是不是这次取数的来源」（=归档开着），不是「这次取到了几条」：
	// 带筛选条件时「区间内没有记录」是一个真实结果，按条数判断会把它错误地回落成
	// 内存里的最近 N 条，把筛选条件之外的请求显示出来。
	archived := snap.Archive.Enabled
	if !archived {
		// 归档未开：回落到进程内最近事件，同一套筛选条件照过（倒序与归档一致）。
		entries = entries[:0]
		for _, e := range snap.Recent {
			if flt.Match(e) {
				entries = append(entries, e)
			}
		}
		if len(entries) > limit {
			entries = entries[:limit]
		}
	}
	if entries == nil {
		entries = []reqlog.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"metrics":  snap,
		"entries":  entries,
		"limit":    limit,
		"archived": archived,
	})
}

// parseTimeParam 解析时间区间参数：unix 秒 / unix 毫秒 / RFC3339 / 本地
// "2006-01-02T15:04"（datetime-local 的原始值）。空串或不可解析都返回零值，
// 零值在 reqlog.Filter 里表示「不过滤这一端」。
func parseTimeParam(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 {
			return time.Time{}
		}
		if n > 1e12 {
			return time.UnixMilli(n)
		}
		return time.Unix(n, 0)
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", v, time.Local); err == nil {
		return t
	}
	return time.Time{}
}

// usage 返回逐请求用量聚合。hours 查询参数控制统计窗口（默认 72，上限 1440=60 天，
// 显式 hours=0 表示全部历史）：汇总卡片/按账号/按模型/时序**全部**按同一窗口统计。
func (p *Panel) usage(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	hours := 72
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			hours = n
		}
	}
	if hours > 1440 {
		hours = 1440
	}
	writeJSON(w, http.StatusOK, p.cfg.Usage.Snapshot(hours, p.nicks()))
}

// usageSave 立即把内存里的用量桶落盘（正常由后台 30s 防抖刷新负责）。
func (p *Panel) usageSave(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	p.cfg.Usage.Save()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) getConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	cfg, err := p.cfg.LoadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load config failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": p.cfg.ConfigPath, "config": cfg})
}

func (p *Panel) saveConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body failed")
		return
	}
	restart, err := p.cfg.SaveConfig(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if restart == nil {
		restart = []string{}
	}
	log.Printf("panel: config saved restart_fields=%d", len(restart))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart_required": restart})
}

func (p *Panel) checkinAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler unavailable")
		return
	}
	results, errs := p.cfg.Scheduler.RunCheckinNow()
	if len(errs) != 0 {
		writeErr(w, checkinErrorStatus(errs[0]), publicErr(errs[0]))
		return
	}
	p.writeOverview(w, map[string]any{"checkin": results})
}

// keepaliveAll 立即对全账号刷一遍 token（保活）。不花积分：只刷新凭证，不调对话。
func (p *Panel) keepaliveAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler unavailable")
		return
	}
	p.cfg.Scheduler.RunRefreshNow()
	p.overview(w, r)
}

func (p *Panel) balanceAll(w http.ResponseWriter, r *http.Request) {
	failed := 0
	for _, st := range p.cfg.Pool.List() {
		if err := p.refreshBalance(st.UID); err != nil {
			failed++
			log.Printf("panel: balance uid=%s failed", st.UID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "failed": failed, "accounts": p.cfg.Pool.List()})
}

func (p *Panel) accountCheckin(w http.ResponseWriter, r *http.Request) {
	uid, ok := p.uidFrom(w, r)
	if !ok {
		return
	}
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler unavailable")
		return
	}
	res, err := p.cfg.Scheduler.CheckinUID(uid)
	if errors.Is(err, scheduler.ErrDisabled) {
		writeErr(w, http.StatusConflict, "account disabled")
		return
	}
	if errors.Is(err, scheduler.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if err != nil {
		writeErr(w, checkinErrorStatus(err), publicErr(err))
		return
	}
	st, _ := p.cfg.Pool.Status(uid)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "status": res.Status, "earned": res.Credits, "account": st,
	})
}

func (p *Panel) accountBalance(w http.ResponseWriter, r *http.Request) {
	uid, ok := p.uidFrom(w, r)
	if !ok {
		return
	}
	if err := p.refreshBalance(uid); err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, scheduler.ErrNotFound) {
			code = http.StatusNotFound
		}
		writeErr(w, code, publicErr(err))
		return
	}
	st, _ := p.cfg.Pool.Status(uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": st})
}

func (p *Panel) accountDisable(w http.ResponseWriter, r *http.Request) {
	uid, ok := p.uidFrom(w, r)
	if !ok {
		return
	}
	if _, exists := p.cfg.Pool.Status(uid); !exists {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Disable(uid, "disabled from panel")
	log.Printf("panel: disabled uid=%s", uid)
	st, _ := p.cfg.Pool.Status(uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": st})
}

// accountEnable 重新启用被禁用的账号（启用后立刻参与选号）。
func (p *Panel) accountEnable(w http.ResponseWriter, r *http.Request) {
	uid, ok := p.uidFrom(w, r)
	if !ok {
		return
	}
	old, exists := p.cfg.Pool.Status(uid)
	if !exists {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if !old.Disabled {
		writeErr(w, http.StatusConflict, "account not disabled")
		return
	}
	if !p.cfg.Pool.Enable(uid) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	log.Printf("panel: enabled uid=%s", uid)
	st, _ := p.cfg.Pool.Status(uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": st})
}

func (p *Panel) accountClear(w http.ResponseWriter, r *http.Request) {
	uid, ok := p.uidFrom(w, r)
	if !ok {
		return
	}
	st, exists := p.cfg.Pool.Status(uid)
	if !exists {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if st.Disabled {
		writeErr(w, http.StatusConflict, "account disabled")
		return
	}
	if !p.cfg.Pool.ClearCooldown(uid) {
		writeErr(w, http.StatusConflict, "not cooling")
		return
	}
	log.Printf("panel: cooldown cleared uid=%s", uid)
	st, _ = p.cfg.Pool.Status(uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": st})
}

func (p *Panel) accountRemove(w http.ResponseWriter, r *http.Request) {
	uid, ok := p.uidFrom(w, r)
	if !ok {
		return
	}
	path, err := authPath(p.cfg.AuthDir, uid)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad uid")
		return
	}
	if _, found := p.cfg.Pool.Remove(uid); !found {
		if _, statErr := os.Stat(path); statErr != nil {
			writeErr(w, http.StatusNotFound, "account not found")
			return
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		writeErr(w, http.StatusInternalServerError, "remove file failed")
		return
	}
	log.Printf("panel: removed uid=%s", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) refreshBalance(uid string) error {
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return scheduler.ErrNotFound
	}
	// 口径感知：国内写积分、国际写美元。老接口 UserEntUsage 对国际号恒返回 0，
	// 面板就会显示成「无额度」——实测国际 Free plan 有 $1 额度、已用 $0.00059。
	u, err := p.cfg.Upstream.UserEntUsageDetail(a)
	if err != nil {
		return err
	}
	p.cfg.Pool.SetBalance(uid, pool.Balance{
		Unit: u.Unit, Remain: u.Remain, Total: u.Total,
		UsedUSD: u.UsedUSD, LimitUSD: u.LimitUSD,
	}, u.Expire)
	return nil
}

// rotateDevice 给账号换一套设备指纹并落盘。
//
// 用途：小浣熊不按设备归组，这个按钮留着不影响什么。**只在用户点按钮时发生**：
// 自动换指纹会打断账号的稳定身份，这里是显式的人工动作。
func (p *Panel) rotateDevice(uid string) error {
	machineID, ok, err := p.cfg.Pool.RotateMachineID(uid)
	if !ok {
		return scheduler.ErrNotFound
	}
	if err != nil {
		return err
	}
	log.Printf("panel: rotate device uid=%s machineId=%s", uid, machineID)
	return nil
}

func (p *Panel) accountRotateDevice(w http.ResponseWriter, r *http.Request) {
	uid, ok := p.uidFrom(w, r)
	if !ok {
		return
	}
	if err := p.rotateDevice(uid); err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, scheduler.ErrNotFound) {
			code = http.StatusNotFound
		}
		writeErr(w, code, publicErr(err))
		return
	}
	st, _ := p.cfg.Pool.Status(uid)
	dev := ""
	if a := p.cfg.Pool.AuthByUID(uid); a != nil {
		dev = a.MachineIDValue()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": st, "machine_id": dev})
}

// rotateDeviceAll 一键给全部账号换指纹（风控按设备维度批量标记时用）。
func (p *Panel) rotateDeviceAll(w http.ResponseWriter, r *http.Request) {
	failed := 0
	for _, st := range p.cfg.Pool.List() {
		if err := p.rotateDevice(st.UID); err != nil {
			failed++
			log.Printf("panel: rotate device uid=%s failed: %v", st.UID, err)
		}
	}
	p.writeOverview(w, map[string]any{"rotated_failed": failed})
}

func (p *Panel) uidFrom(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid := r.PathValue("uid")
	if !validUID(uid) {
		writeErr(w, http.StatusBadRequest, "bad uid")
		return "", false
	}
	return uid, true
}

var uidRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func validUID(s string) bool { return uidRE.MatchString(s) }

// authPath 只允许 auths/{uid}.json，拒绝路径穿越。
func authPath(dir, uid string) (string, error) {
	if dir == "" || !validUID(uid) {
		return "", fmt.Errorf("bad uid")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	full := filepath.Join(absDir, auth.FileNameFor(uid))
	rel, err := filepath.Rel(absDir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("bad uid")
	}
	return full, nil
}

func checkinErrorStatus(err error) int {
	var be *upstream.BusinessError
	if errors.As(err, &be) {
		return http.StatusConflict
	}
	return http.StatusBadGateway
}

func publicErr(err error) string {
	if err == nil {
		return ""
	}
	var ue *upstream.Error
	if errors.As(err, &ue) {
		msg := fmt.Sprintf("upstream %s http %d", ue.Kind, ue.Status)
		// 带上上游原话/我们翻的处置：没有它，「刷 token 失败」只剩一个状态码，
		// 运维看不出是「该重登」还是「网络抖动」（换票被拒的四个码就是这么翻出来的）。
		if ue.Msg != "" {
			msg += "：" + ue.Msg
		}
		return msg
	}
	return err.Error()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
