package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
)

// 上游地址（可被 config.upstream 覆盖）。
const (
	LLMBase    = "https://xiaohuanxiong.com/api/web/llm/v2"
	AuthBase   = "https://xiaohuanxiong.com/api/web/auth/v1"
	PointsBase = "https://xiaohuanxiong.com/api/web/points/v1"
	Origin     = "https://xiaohuanxiong.com"

	// ChatPath / CatalogPath 挂在 LLM 网关下；RefreshPath 挂在鉴权域名下。
	ChatPath        = "/chat/completions"
	CatalogPath     = "/model_catalog"
	RefreshPath     = "/refresh"
	LoginCodePath   = "/login_with_authorization_code"
	BalancePath     = "/balance"
	UserInfoPath    = "/user_info"
	EntitlementPath = "/entitlement_info"
	// 每日 300 分是服务端 00:00 自动发放、午夜自动清零（bills 流水实锤），
	// 不存在「调 setting_info 触发发放」这种客户端侧开关——旧 DailyGrantPath
	// 是对抓包的误读。登录领取动作是 desktop 域的 grant（幂等，领过了回
	// granted:false），流水查询是 points 域的 bills。
	DesktopBase     = Origin + "/api/web/desktop/v1"
	PointsGrantPath = "/login/points/grant"
	PointsBillsPath = "/bills"
	// limit 必须给足：流水按时间倒序，重度使用一天几十条 -N 消费记录会把
	// 今日 +300 的 daily_grant 挤出前 10 条，签到状态就会误判 false。
	PointsBillsQuery = "paging.limit=100&type=all&paging.offset=0"

	// RegionGlobal / RegionCN 保留基线的地区常量：小浣熊只有一个站
	// （xiaohuanxiong.com），面板上的「地区」选择已注释掉（index.html 的 .realm-row），
	// login/start 固定按 RegionCN 建会话。等官方出国际版客户端再放开那块 UI，
	// pool 的分区选号/美元额度逻辑一直在（休眠状态），不需要重写。
	RegionGlobal = "cn"
	RegionCN     = "cn"

	// DefaultConfigName 是上游默认对话模型（目录里拿不到时用它兜底）。
	// v1.1.0 抓包确认：model_catalog 的 default_model = raccoon-8c4485
	// （raccoon-chat-ml-5-5 不在目录里，但上游实测仍接受，保留作别名目标）。
	DefaultConfigName     = "raccoon-8c4485"
	DefaultConfigNameIntl = DefaultConfigName

	// DefaultClientVersion 是出站 x-client-version 头的内置值（v1.1.0 抓包实测）；
	// 官方升版本后可用 config.upstream.client_version 覆盖，不必重编译。
	DefaultClientVersion = "v1.1.0"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone          ErrKind = iota // 与账号无关（模型/参数/业务码）
	ErrPlanLimit                    // 额度不足 → 计划硬冷却
	ErrSoftRate                     // 限流 → 软冷却
	ErrSessionDead                  // 登录态失效 → 连续达阈值才禁用
	ErrNotFound                     // 404 → 短冷却
	ErrServer                       // 5xx → 熔断计数
	ErrClient                       // 其余 4xx → 降权（本仓按「不罚号」处理）
	ErrBusy                         // 上游排队 → 换号不罚号
	ErrReasoningLoop                // 思考链复读（模型侧问题）→ 中断且不罚号
	ErrGateway                      // 502/504 网关超时 → 不罚号、不换号
)

// String 供日志与面板展示。
func (k ErrKind) String() string {
	switch k {
	case ErrPlanLimit:
		return "plan_limit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	case ErrBusy:
		return "busy"
	case ErrReasoningLoop:
		return "reasoning_loop"
	case ErrGateway:
		return "gateway"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind           ErrKind
	Status         int
	Msg            string
	Busy           bool // 排队态：可原地重试
	RetryAfterSecs int64
	// ServiceUnavailable = 排队体里 serviceAvailable:false：等也没用，立刻回错。
	ServiceUnavailable bool
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return e.Msg
	}
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// BusinessError 是 HTTP 成功但上游业务拒绝（code != 0）。
type BusinessError struct {
	Code    int
	Message string
}

func (e *BusinessError) Error() string { return e.Message }

// ErrEmptyStream 上游一个字都没吐（可换号重试，但不罚号）。
var ErrEmptyStream = errors.New("上游返回空流")

// ErrCheckinAlready 今日已领取（上游幂等，不是失败）。
var ErrCheckinAlready = errors.New("今日已领取")

// ErrCodeExpired 授权码已失效：提示重新发起，不要重试。
var ErrCodeExpired = errors.New("授权码已失效，请重新发起登录")

// CodeExpired 是上游「授权码失效」的业务码。
const CodeExpired = 200035

// IsCheckinRateLimited 小浣熊签到没有专用限流码。
func IsCheckinRateLimited(error) bool { return false }

// 失败分类的判据表（HTTP 与流内共用，别各写一份）。
//
// 教训来自同类实现的审计记录：`insufficient` 单独出现太宽——"context window is
// insufficient for this model" 也会命中，于是给一个只是上下文太长的**好号**写 12 小时
// 计划冷却；而 `token` 单独出现同样过宽——"max_tokens exceeded" 会把号记成登录态失效，
// 连续几次就自动禁用。两条都是「静默误伤可用账号」，比漏判贵得多。
//
// 方向：宁可漏判（不罚号、只换号重试），不可误伤。

// quotaSignal 文案里是否**确实**在说额度/余额不足。
func quotaSignal(lower string) bool {
	if strings.Contains(lower, "14018") || strings.Contains(lower, "credits exhausted") {
		return true
	}
	if strings.Contains(lower, "insufficient") {
		for _, w := range []string{"point", "credit", "balance", "quota", "余额", "积分", "额度"} {
			if strings.Contains(lower, w) {
				return true
			}
		}
	}
	for _, pair := range [][2]string{
		{"积分", "不足"}, {"积分", "耗尽"}, {"额度", "不足"}, {"额度", "耗尽"},
		{"余额", "不足"}, {"余额", "耗尽"}, {"quota", "exceeded"}, {"quota", "exhausted"},
	} {
		if strings.Contains(lower, pair[0]) && strings.Contains(lower, pair[1]) {
			return true
		}
	}
	return false
}

// sessionDeadSignal 文案里是否**确实**在说登录态失效（不含泛化的 "token"）。
func sessionDeadSignal(lower string) bool {
	for _, s := range []string{
		"unauthorized", "invalid token", "token expired", "token is expired",
		"token_expired", "authorization_verify_error", "invalid_grant",
		"登录态已失效", "凭证已失效", "请重新登录",
	} {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// Classify 按 HTTP 状态与正文判定错误类别。
//
// 文案判据与流内的 classifyErrorText 共用 quotaSignal / sessionDeadSignal（见上）：
// 各写一份必然漂移，同一报文会在 HTTP 与 SSE 两条路径上被判成不同类别。
func Classify(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	switch {
	// 认证类**先判**：403 可能只是权限问题，落到下面的额度文案会白白封号。
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ErrSessionDead
	case status == http.StatusTooManyRequests:
		return ErrSoftRate
	case status == http.StatusGatewayTimeout:
		return ErrGateway
	case status == http.StatusRequestTimeout || status == 499:
		// 499（nginx「client closed request」）/408：调用方在响应完成前断开，多为请求体
		// 过大或上游超时。换号重发只会照样失败、把每个号都记一次错，所以归「客户端侧」。
		return ErrClient
	case status == http.StatusBadGateway:
		return ErrGateway
	case status >= 500:
		return ErrServer
	case status == http.StatusNotFound:
		return ErrNotFound
	}
	if queueUnavailable(body) {
		return ErrBusy
	}
	if quotaSignal(lower) || status == http.StatusPaymentRequired {
		return ErrPlanLimit
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

// Client 访问小浣熊上游。四个基址都可由配置/测试覆盖。
type Client struct {
	// HTTP 用于短 JSON 请求（续期/模型/签到/额度），有总超时兜底。
	HTTP *http.Client
	// StreamHTTP 用于 SSE 流式对话：不设总超时，只用 ResponseHeaderTimeout
	// 兜底「上游一直不返首字节」的悬挂。
	StreamHTTP *http.Client

	timeoutMu   sync.RWMutex
	idleTimeout time.Duration

	hostMu sync.RWMutex
	llm    string
	auth   string
	points string
	origin string

	uaMu          sync.RWMutex
	userAgent     string
	clientVersion string

	catMu   sync.RWMutex
	catalog []ModelInfo
	catAt   time.Time

	flightMu sync.Mutex
	flights  map[string]*refreshFlight
}

// New 生产默认值：连接池共享，短请求 120s 兜底、长流只看首字节超时。
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		HTTP:       &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr},
		llm:        LLMBase,
		auth:       AuthBase,
		points:     PointsBase,
		origin:     Origin,
		flights:    map[string]*refreshFlight{},
	}
}

// SetTimeouts 热改超时：short 是短请求总超时，header 是首字节超时，idle 是流内空闲上限。
func (c *Client) SetTimeouts(short, header, idle time.Duration) {
	c.timeoutMu.Lock()
	defer c.timeoutMu.Unlock()
	if short > 0 && c.HTTP != nil {
		c.HTTP.Timeout = short
	}
	if header > 0 {
		for _, hc := range []*http.Client{c.HTTP, c.StreamHTTP} {
			if hc == nil {
				continue
			}
			if tr, ok := hc.Transport.(*http.Transport); ok {
				tr.ResponseHeaderTimeout = header
			}
		}
	}
	c.idleTimeout = idle
}

// SetIdentity 覆盖出站 User-Agent 与客户端版本头（空 = 用内置默认）。
func (c *Client) SetIdentity(userAgent, clientVersion string) {
	c.uaMu.Lock()
	c.userAgent = strings.TrimSpace(userAgent)
	v := strings.TrimSpace(clientVersion)
	if v != "" && !strings.HasPrefix(v, "v") {
		v = "v" + v // 配置里写 1.1.0 和写 v1.1.0 都行，头里统一带 v（照客户端实测）
	}
	c.clientVersion = v
	c.uaMu.Unlock()
}

// currentVersion 返回 x-client-version 头取值：配置覆盖优先，否则内置抓包实测值。
func (c *Client) currentVersion() string {
	c.uaMu.RLock()
	defer c.uaMu.RUnlock()
	if c.clientVersion != "" {
		return c.clientVersion
	}
	return DefaultClientVersion
}

// SetAgentHosts 覆盖推理网关与鉴权基址（config.upstream.llm_base / auth_base）。
func (c *Client) SetAgentHosts(llm, authBase string) {
	c.hostMu.Lock()
	c.llm = orDefault(strings.TrimRight(strings.TrimSpace(llm), "/"), LLMBase)
	c.auth = orDefault(strings.TrimRight(strings.TrimSpace(authBase), "/"), AuthBase)
	c.hostMu.Unlock()
}

// SetAPIHosts 覆盖积分与主站基址（测试与自建镜像用）。
func (c *Client) SetAPIHosts(points, origin string) {
	c.hostMu.Lock()
	c.points = orDefault(strings.TrimRight(strings.TrimSpace(points), "/"), PointsBase)
	c.origin = orDefault(strings.TrimRight(strings.TrimSpace(origin), "/"), Origin)
	c.hostMu.Unlock()
}

// AgentHosts 返回当前网关基址（面板展示「生效地址」用）。
func (c *Client) AgentHosts() (llm, authBase string) {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.llm, c.auth
}

// Origins 返回积分与主站基址（面板展示用）。
func (c *Client) Origins() (points, origin string) {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.points, c.origin
}

func (c *Client) llmBase() string {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.llm
}

func (c *Client) authBase() string {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.auth
}

func (c *Client) pointsBase() string {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.points
}

func (c *Client) originBase() string {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.origin
}

// desktopBase 桌面端专属域（登录领取积分等）：挂在主站 origin 下，
// 与积分/鉴权域同生共死，config 覆盖主站时一起跟着走。
func (c *Client) desktopBase() string {
	return c.originBase() + "/api/web/desktop/v1"
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) streamClient() *http.Client {
	if c.StreamHTTP != nil {
		return c.StreamHTTP
	}
	return c.httpClient()
}

func (c *Client) currentUA() string {
	c.uaMu.RLock()
	defer c.uaMu.RUnlock()
	if c.userAgent != "" {
		return c.userAgent
	}
	return "xhx2api-panel"
}

type apiResponse struct {
	Status int
	Body   []byte
}

// do 发一次请求并读回响应体（上限 4MiB）。
func (c *Client) do(req *http.Request) (*apiResponse, error) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.currentUA())
	}
	c.timeoutMu.RLock()
	resp, err := c.httpClient().Do(req)
	c.timeoutMu.RUnlock()
	if err != nil {
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "无法连接小浣熊，请检查网络或代理设置"}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "读取小浣熊响应失败"}
	}
	return &apiResponse{Status: resp.StatusCode, Body: raw}, nil
}

// doJSON 发请求：body 非 nil 时按 JSON 序列化，headers 原样写入。
func (c *Client) doJSON(method, url string, body any, headers map[string]string) (*apiResponse, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return c.do(req)
}

// payloadOf 校验 2xx 并拆掉 {code,message,data} 信封，返回 data 节点。
func payloadOf(resp *apiResponse, action string) (map[string]any, error) {
	if resp.Status < 200 || resp.Status >= 300 {
		hint := ""
		switch resp.Status {
		case 401, 403:
			hint = "，登录态可能已失效"
		case 429:
			hint = "，请稍后重试"
		}
		return nil, &Error{
			Kind:   Classify(resp.Status, string(resp.Body)),
			Status: resp.Status,
			Msg:    fmt.Sprintf("小浣熊 %s失败（HTTP %d）%s: %s", action, resp.Status, hint, truncate(errorText(resp.Body), 200)),
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(resp.Body))
	decoder.UseNumber()
	var envelope map[string]any
	if err := decoder.Decode(&envelope); err != nil {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: fmt.Sprintf("小浣熊 %s未返回有效 JSON 对象", action)}
	}
	if code, ok := intOf(envelope["code"]); ok && code != 0 {
		return nil, &BusinessError{Code: code, Message: orDefault(text(envelope, "message", "msg"), fmt.Sprintf("小浣熊 %s被拒绝（code=%d）", action, code))}
	}
	data, _ := envelope["data"].(map[string]any)
	if data == nil {
		data = map[string]any{}
	}
	return data, nil
}

// errorText 从错误体里取人类可读文案。
func errorText(body []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if decoder.Decode(&payload) != nil {
		return strings.TrimSpace(string(body))
	}
	if msg := text(payload, "message", "msg", "detail"); msg != "" {
		return msg
	}
	if inner, ok := payload["error"].(map[string]any); ok {
		return text(inner, "message")
	}
	return strings.TrimSpace(string(body))
}

// ---------------------------------------------------------------------------
// 续期

type refreshFlight struct {
	done  chan struct{}
	token string
	exp   int64
	uid   string
	err   error
}

// RefreshToken 无条件续期一次。
func (c *Client) RefreshToken(a *auth.Auth) error {
	_, err := c.refresh(a)
	return err
}

// RefreshTokenIfNeeded 仅在 skew 内将过期时续期；返回是否真的续了。
func (c *Client) RefreshTokenIfNeeded(a *auth.Auth, skew time.Duration) (bool, error) {
	if !a.NeedsRefresh(skew) {
		return false, nil
	}
	return c.refresh(a)
}

// refresh 单飞：同一份 refresh_token 并发只打一次上游（上游每次轮换，并发会让一副票作废）。
func (c *Client) refresh(a *auth.Auth) (bool, error) {
	refreshToken := a.RefreshTokenValue()
	if refreshToken == "" {
		return false, &Error{Kind: ErrClient, Status: 400, Msg: "该账号没有 refresh_token，无法续期，请重新登录"}
	}
	key := a.UID + "|" + refreshToken
	c.flightMu.Lock()
	if c.flights == nil {
		c.flights = map[string]*refreshFlight{}
	}
	if flight, ok := c.flights[key]; ok {
		c.flightMu.Unlock()
		<-flight.done
		if flight.err != nil {
			return false, flight.err
		}
		a.SetTokens(flight.token, "", flight.exp)
		applyRefreshedUID(a, flight.uid)
		return true, nil
	}
	flight := &refreshFlight{done: make(chan struct{})}
	c.flights[key] = flight
	c.flightMu.Unlock()

	token, refresh, exp, uid, err := c.refreshOnce(a)
	flight.token, flight.exp, flight.uid, flight.err = token, exp, uid, err
	close(flight.done)
	c.flightMu.Lock()
	delete(c.flights, key)
	c.flightMu.Unlock()
	if err != nil {
		return false, err
	}
	a.SetTokens(token, refresh, exp)
	applyRefreshedUID(a, uid)
	return true, nil
}

// applyRefreshedUID 用续期响应补 uid：小浣熊的 access token 未必是 JWT（也可能只是不透明串），
// uid 只有上游自己知道，缺了就没法落盘成账号。
func applyRefreshedUID(a *auth.Auth, uid string) {
	if uid == "" || a.UID != "" {
		return
	}
	a.UID = uid
	if a.Nickname == "" {
		a.Nickname = uid
	}
}

// refreshOnce 真的打一次续期接口并返回新票（不写回账号）。
func (c *Client) refreshOnce(a *auth.Auth) (token, refresh string, exp int64, uid string, err error) {
	resp, err := c.doJSON(http.MethodPost, c.authBase()+RefreshPath,
		map[string]string{"refresh_token": a.RefreshTokenValue()}, c.clientIDHeaders())
	if err != nil {
		return "", "", 0, "", err
	}
	if resp.Status == 401 || resp.Status == 403 {
		return "", "", 0, "", &Error{Kind: ErrSessionDead, Status: 401, Msg: "小浣熊刷新令牌已失效，请重新登录该账号"}
	}
	data, err := payloadOf(resp, "凭证续期")
	if err != nil {
		return "", "", 0, "", err
	}
	token = text(data, "access_token", "accessToken", "token")
	if token == "" {
		return "", "", 0, "", &Error{Kind: ErrServer, Status: 502, Msg: "小浣熊续期响应缺少令牌，旧凭证未被覆盖"}
	}
	uid = text(data, "user_id", "uid", "userId")
	if uid != "" && a.UID != "" && uid != a.UID {
		return "", "", 0, "", &Error{Kind: ErrServer, Status: 502, Msg: "小浣熊续期返回了不同账号，旧凭证未被覆盖"}
	}
	// 上游可能不轮换 refresh_token：没有新值就沿用旧的（SetTokens 收到空串即不动它）。
	refresh = text(data, "refresh_token", "refreshToken")
	for _, key := range []string{"expires_at", "expiresAt", "expire_time", "expireTime"} {
		if ms := millisOf(data[key]); ms > 0 {
			exp = ms
			break
		}
	}
	return token, refresh, exp, uid, nil
}

// ---------------------------------------------------------------------------
// 用户资料

// GetUserInfo 用上游 GET {auth_base}/user_info 补齐 uid/昵称（抓包实测存在，
// 200 返回 {data:{id,name,...}}）；接口不可用时退回 JWT claims，保证不比以前差。
func (c *Client) GetUserInfo(a *auth.Auth) (uid, nickname, enterpriseID string, err error) {
	if token := a.JWT(); token != "" {
		if resp, uerr := c.doJSON(http.MethodGet, c.authBase()+UserInfoPath, nil,
			map[string]string{"Accept": "application/json", "Authorization": "Bearer " + token}); uerr == nil {
			if data, perr := payloadOf(resp, "用户资料"); perr == nil {
				uid = text(data, "id", "user_id", "uid")
				nickname = text(data, "name", "nickname")
				enterpriseID = text(data, "office_org_name", "org_code")
				if uid != "" {
					return uid, nickname, enterpriseID, nil
				}
			}
		}
	}
	token := a.JWT()
	uid = auth.JWTUserID(token)
	nickname = a.Nickname
	if uid == "" && a.UID != "" {
		return a.UID, nickname, "", nil
	}
	if uid == "" {
		return "", "", "", &Error{Kind: ErrSessionDead, Status: 401, Msg: "无法从 token 解析用户标识，请重新登录"}
	}
	return uid, nickname, "", nil
}

// ---------------------------------------------------------------------------
// 推理

// ChatStream 把 OpenAI 请求体发给小浣熊的 chat/completions。
//
// 上游就是 OpenAI 形状：body 原样转发（model 已由 handler 定成规范 id），
// 返回未读的 SSE 响应体供 Stream/Aggregate 消费；非 2xx 时返回分类后的 *Error。
//
// turnTitle 非空时作为 X-RACCOON-Turn-Title 发出去。这不是装饰：上游会把标题写进
// 积分流水的 event_name（「任务消耗 · <title>」），于是每一笔扣费都能反查到是哪一次
// 请求——积分对账（见 upstream.RecentBills / panel.reconcileCredits）完全靠它。
func (c *Client) ChatStream(a *auth.Auth, body []byte, turnTitle string) (io.ReadCloser, int, []byte, error) {
	url := c.llmBase() + ChatPath
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 500, nil, &Error{Kind: ErrServer, Status: 500, Msg: "构造小浣熊上游请求失败"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Accept-Encoding", "identity")
	for key, value := range c.clientIDHeaders() {
		req.Header.Set(key, value)
	}
	req.Header.Set("User-Agent", c.currentUA())
	// 标题用于积分对账：上游把它拼进扣费流水的 event_name（「任务消耗 · <title>」）。
	// 头名沿用抓包里的写法（v1.1.0 客户端发 X-RACCOON-Title），空标题就不发。
	if title := strings.TrimSpace(turnTitle); title != "" {
		req.Header.Set("X-RACCOON-Title", title)
	}
	if token := a.JWT(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	c.timeoutMu.RLock()
	resp, err := c.streamClient().Do(req)
	c.timeoutMu.RUnlock()
	if err != nil {
		return nil, 502, nil, &Error{Kind: ErrServer, Status: 502, Msg: "无法连接小浣熊，请检查网络或代理设置"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		msg := truncate(errorText(raw), 300)
		if msg == "" {
			msg = fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode)
		}
		return nil, resp.StatusCode, raw, &Error{Kind: kind, Status: resp.StatusCode, Msg: msg,
			RetryAfterSecs: retryAfterOf(string(raw)), ServiceUnavailable: queueUnavailable(string(raw))}
	}

	// stream:false 时上游回的是普通 JSON（不是 SSE）：包成一帧 SSE 交给同一套管道，
	// 下游（handler/Aggregate）就只用认一种线上形状。
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, 502, nil, &Error{Kind: ErrServer, Status: 502, Msg: "读取上游响应失败"}
		}
		if event := parseSSELine(string(raw)); event.Kind == sseError {
			return nil, event.Status, raw, &Error{Kind: event.Class, Status: event.Status,
				Msg: orDefault(event.Message, event.Raw), RetryAfterSecs: event.RetryAfterSecs,
				ServiceUnavailable: event.ServiceUnavailable}
		}
		frame := deltaFrameOf(raw)
		stream := "data: " + string(frame) + "\n\ndata: [DONE]\n\n"
		return io.NopCloser(strings.NewReader(stream)), resp.StatusCode, nil, nil
	}

	// HTTP 200 也可能是错误：上游会用一帧 {"error":{...}} 表达限流/额度。
	// 只看状态码会把它当成功，账号也就没法换——所以先窥一帧再交出去。
	br := newPeekReader(resp.Body)
	first, err := br.peekFrame()
	if err != nil {
		_ = resp.Body.Close()
		return nil, 502, nil, &Error{Kind: ErrServer, Status: 502, Msg: "读取上游首帧失败"}
	}
	if first.Kind == sseError {
		_ = resp.Body.Close()
		return nil, first.Status, []byte(first.Raw), &Error{Kind: first.Class, Status: first.Status,
			Msg: orDefault(first.Message, first.Raw), RetryAfterSecs: first.RetryAfterSecs,
			ServiceUnavailable: first.ServiceUnavailable}
	}
	return br.stream(), resp.StatusCode, nil, nil
}

// PrepareBody 是入站体的轻量规整钩子：小浣熊原样透传，这里不改任何字段。
func PrepareBody(src []byte) []byte { return src }

var (
	retryAfterRE   = regexp.MustCompile(`"retryAfterSeconds"\s*:\s*(\d+)`)
	waitTimeRE     = regexp.MustCompile(`"waitTime"\s*:\s*(\d+)`)
	serviceAvailRE = regexp.MustCompile(`"serviceAvailable"\s*:\s*false`)
)

// queueUnavailable 看排队体里的 serviceAvailable:false——上游自己说此刻不可服务。
func queueUnavailable(raw string) bool {
	flat := strings.ReplaceAll(raw, `\`, "")
	return serviceAvailRE.MatchString(flat)
}

// retryAfterOf 抓上游建议的重试间隔（秒）。
func retryAfterOf(raw string) int64 {
	flat := strings.ReplaceAll(raw, `\`, "")
	for _, re := range []*regexp.Regexp{retryAfterRE, waitTimeRE} {
		if m := re.FindStringSubmatch(flat); len(m) == 2 {
			if n, err := strconv.ParseInt(m[1], 10, 64); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// millisOf 把数字/数字字符串归一成毫秒时间戳（0 = 没有）。
func millisOf(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return normalizeMillis(int64(typed))
	case int64:
		return normalizeMillis(typed)
	case json.Number:
		if n, err := typed.Int64(); err == nil {
			return normalizeMillis(n)
		}
	case string:
		trimmed := strings.TrimSpace(typed)
		if n, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			return normalizeMillis(n)
		}
		if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
			return parsed.UnixMilli()
		}
	}
	return 0
}

func normalizeMillis(value int64) int64 {
	if value <= 0 {
		return 0
	}
	if value < 100_000_000_000 {
		return value * 1000
	}
	return value
}

// truncate 截断到 n 个 rune（不切坏多字节字符）。
func truncate(value string, n int) string {
	runes := []rune(value)
	if len(runes) <= n {
		return value
	}
	return string(runes[:n])
}

// text 取第一个非空的字符串/数字字段。
func text(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		switch value := payload[key].(type) {
		case string:
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		case json.Number:
			return value.String()
		case float64:
			return strconv.FormatFloat(value, 'f', -1, 64)
		}
	}
	return ""
}

// intOf 把 JSON 数字/数字字符串归一成 int。
func intOf(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case json.Number:
		if n, err := typed.Int64(); err == nil {
			return int(n), true
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
			return n, true
		}
	}
	return 0, false
}

// deltaFrameOf 把非流式 chat.completion 摊成同形状的一帧 chunk（message → delta），
// 让下游只用处理增量帧。解析失败时原样返回，交给 Aggregate 报空流。
func deltaFrameOf(raw []byte) []byte {
	var frame map[string]any
	if err := json.Unmarshal(raw, &frame); err != nil {
		return raw
	}
	choices, _ := frame["choices"].([]any)
	if len(choices) == 0 {
		return raw
	}
	if choice, ok := choices[0].(map[string]any); ok {
		if message, ok := choice["message"].(map[string]any); ok {
			choice["delta"] = message
			delete(choice, "message")
			if _, has := choice["index"]; !has {
				choice["index"] = 0
			}
		}
	}
	frame["object"] = "chat.completion.chunk"
	out, err := json.Marshal(frame)
	if err != nil {
		return raw
	}
	return out
}
