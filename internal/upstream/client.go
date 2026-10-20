package upstream

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/qoder2api-panel/internal/auth"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone          ErrKind = iota // 与账号无关（模型/参数/业务码）
	ErrPlanLimit                    // 额度不足/套餐不支持 → 计划硬冷却
	ErrSoftRate                     // 限流 → 软冷却
	ErrSessionDead                  // 登录态失效 → 连续达阈值才禁用
	ErrNotFound                     // 404 → 短冷却
	ErrServer                       // 5xx → 熔断计数
	ErrClient                       // 其余 4xx → 降权（本仓按「不罚号」处理）
	ErrBusy                         // 上游排队 → 换号不罚号
	ErrReasoningLoop                // 思考链复读（模型侧问题）→ 中断且不罚号
	ErrGateway                      // 502/504 网关超时（请求太大/上游排队超时）→ 不罚号、不换号
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
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// BusinessError 是 HTTP 成功但上游业务拒绝（签到复核等内部判定也用它）。
type BusinessError struct {
	Code    int
	Message string
}

func (e *BusinessError) Error() string { return e.Message }

// ErrEmptyStream 上游一个字都没吐（可换号重试，但不罚号）。
var ErrEmptyStream = errors.New("上游返回空流")

// ErrCheckinAlready 今日已领取（上游幂等竞态，不是失败）。
var ErrCheckinAlready = errors.New("今日已领取")

// IsCheckinRateLimited Qoder 的签到限流没有专用业务码（换设备指纹这条兜底路径保留签名）。
func IsCheckinRateLimited(err error) bool { return false }

// Classify 按 HTTP 状态与正文判定错误类别。
//
// 顺序：传输层信号（401/429/5xx）→ 排队 → 额度关键词 → 4xx 参数问题。
// 额度关键词排在传输层之后，避免 500 里的 "quota" 文案被记成额度不足。
func Classify(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	switch {
	case status == http.StatusUnauthorized:
		return ErrSessionDead
	case status == http.StatusTooManyRequests:
		return ErrSoftRate
	case status == http.StatusBadGateway || status == http.StatusGatewayTimeout:
		// 502/504 是网关/上游自己超时（实测 1M token 请求 150s 后 504）：账号没问题，
		// 重发同一份巨体也没意义，交给调用方如实回错。
		return ErrGateway
	case status >= 500:
		return ErrServer
	case status == http.StatusNotFound:
		return ErrNotFound
	}
	if messageHasQueue(body) {
		return ErrBusy
	}
	for _, code := range quotaCodes() {
		if rawHasCode(body, code) {
			return ErrPlanLimit
		}
	}
	for _, signal := range []string{"insufficient", "no_quota", "quota_exceed", "exceed_quota", "pricingurl", "subscription", "upgrade"} {
		if strings.Contains(lower, signal) {
			return ErrPlanLimit
		}
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

// Client 访问 Qoder 上游。网关/openapi 基址可由配置覆盖；其余主机固定。
type Client struct {
	// HTTP 用于短 JSON 请求（续期/模型/签到/额度），有总超时兜底。
	HTTP *http.Client
	// StreamHTTP 用于 SSE 流式对话：不设总超时，只用 ResponseHeaderTimeout
	// 兜底「上游一直不返首字节」的悬挂。
	StreamHTTP *http.Client

	// timeoutMu 保护超时（面板热改配置会调 SetTimeouts，请求路径并发读）。
	timeoutMu   sync.RWMutex
	idleTimeout time.Duration

	// hostMu 保护基址（热改与请求路径并发读写）。
	hostMu    sync.RWMutex
	gwGlobal  string
	gwCN      string
	apiGlobal string
	apiCN     string

	uaMu      sync.RWMutex
	userAgent string

	catMu   sync.RWMutex
	catalog map[string][]ModelInfo // realm → 条目
	catAt   map[string]time.Time

	flightMu sync.Mutex
	flights  map[string]*refreshFlight

	// chatBasesOverride 测试注入点：非空时用这组端点替换默认候选。
	chatBasesOverride []string
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
		gwGlobal:   AgentHost,
		gwCN:       GatewayCN,
		apiGlobal:  OpenAPIGlobal,
		apiCN:      OpenAPICN,
		catalog:    map[string][]ModelInfo{},
		catAt:      map[string]time.Time{},
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

// SetIdentity 覆盖出站 User-Agent（空 = 用内置 UA）。
func (c *Client) SetIdentity(userAgent, _ string) {
	c.uaMu.Lock()
	c.userAgent = strings.TrimSpace(userAgent)
	c.uaMu.Unlock()
}

// SetAgentHosts 覆盖网关基址：global 是国际版网关，cn 是国内版网关。
// 参数顺序与旧面板的 SetAgentHosts(cn, intl) 不同——调用点只有 cmd/server 一处。
func (c *Client) SetAgentHosts(global, cn string) {
	c.hostMu.Lock()
	c.gwGlobal = orDefault(strings.TrimSpace(global), AgentHost)
	c.gwCN = orDefault(strings.TrimSpace(cn), GatewayCN)
	c.hostMu.Unlock()
}

// SetAPIHosts 覆盖 openapi 基址（测试与自建镜像用；留空回落内置常量）。
func (c *Client) SetAPIHosts(global, cn string) {
	c.hostMu.Lock()
	c.apiGlobal = orDefault(strings.TrimSpace(global), OpenAPIGlobal)
	c.apiCN = orDefault(strings.TrimSpace(cn), OpenAPICN)
	c.hostMu.Unlock()
}

// AgentHosts 返回当前网关基址（面板展示「生效地址」用）。
func (c *Client) AgentHosts() (global, cn string) {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.gwGlobal, c.gwCN
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// gatewayBase 按地区取推理网关基址（已去掉结尾斜杠）。
func (c *Client) gatewayBase(realm string) string {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	if ifCN(realm) {
		return strings.TrimRight(c.gwCN, "/")
	}
	return strings.TrimRight(c.gwGlobal, "/")
}

// apiBase 按地区取 openapi 基址（已去掉结尾斜杠）。
func (c *Client) apiBase(realm string) string {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	if ifCN(realm) {
		return strings.TrimRight(c.apiCN, "/")
	}
	return strings.TrimRight(c.apiGlobal, "/")
}

// inferenceBase 国际版按令牌前缀分流：jt- 走 api2，其余走配置的国际网关。
func (c *Client) inferenceBase(realm, token string) string {
	base := c.gatewayBase(realm)
	if !ifCN(realm) && strings.HasPrefix(token, "jt-") {
		return API2Global
	}
	return base
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
	return "qoder-local-proxy"
}

type apiResponse struct {
	Status int
	Body   []byte
}

// do 发一次请求并读回响应体（上限 4MiB，防止病态响应打爆内存）。
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
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "无法连接 Qoder，请检查网络或代理设置"}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "读取 Qoder 响应失败"}
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

// payloadOf 校验 2xx 并把响应体解析成 JSON 对象。
func payloadOf(resp *apiResponse, action string) (map[string]any, error) {
	if resp.Status < 200 || resp.Status >= 300 {
		hint := ""
		switch resp.Status {
		case 401, 403:
			hint = "，请确认凭证有效且地区选择正确"
		case 429:
			hint = "，请稍后重试"
		}
		return nil, &Error{
			Kind:   Classify(resp.Status, string(resp.Body)),
			Status: resp.Status,
			Msg:    fmt.Sprintf("Qoder %s失败（HTTP %d）%s", action, resp.Status, hint),
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(resp.Body))
	decoder.UseNumber()
	var data map[string]any
	if err := decoder.Decode(&data); err != nil {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: fmt.Sprintf("Qoder %s未返回有效 JSON 对象", action)}
	}
	return data, nil
}

// ---------------------------------------------------------------------------
// 续期

type refreshFlight struct {
	done  chan struct{}
	token string
	exp   int64
	err   error
}

// RefreshToken 无条件续期一次：PAT 型走 jobToken/exchange，其余走 deviceToken/jobToken refresh。
func (c *Client) RefreshToken(a *auth.Auth) error {
	_, err := c.refresh(a)
	return err
}

// RefreshTokenIfNeeded 仅在 skew 内将过期（或无 expiry）时续期；返回是否真的续了。
func (c *Client) RefreshTokenIfNeeded(a *auth.Auth, skew time.Duration) (bool, error) {
	if !a.NeedsRefresh(skew) {
		return false, nil
	}
	return c.refresh(a)
}

func (c *Client) refresh(a *auth.Auth) (bool, error) {
	key := fmt.Sprintf("%s|%s|%s", a.UID, a.JWT(), a.RefreshTokenValue())
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
		return true, nil
	}
	flight := &refreshFlight{done: make(chan struct{})}
	c.flights[key] = flight
	c.flightMu.Unlock()

	token, refresh, exp, err := c.refreshOnce(a)
	flight.token, flight.exp, flight.err = token, exp, err
	close(flight.done)
	c.flightMu.Lock()
	delete(c.flights, key)
	c.flightMu.Unlock()
	if err != nil {
		return false, err
	}
	a.SetTokens(token, refresh, exp)
	return true, nil
}

// refreshOnce 真的打一次续期接口并返回新票（不写回账号）。
func (c *Client) refreshOnce(a *auth.Auth) (token, refresh string, exp int64, err error) {
	realm := a.Realm()
	var resp *apiResponse
	if pat, ok := a.PAT(); ok {
		resp, err = c.doJSON(http.MethodPost, c.apiBase(realm)+ExchangePath,
			map[string]any{"personal_token": pat}, OpenAPIHeaders(""))
		if err != nil {
			return "", "", 0, err
		}
		data, derr := payloadOf(resp, "PAT 换取令牌")
		if derr != nil {
			if e, ok := derr.(*Error); ok && (e.Status == 401 || e.Status == 403) {
				return "", "", 0, &Error{Kind: ErrSessionDead, Status: 401, Msg: "Qoder PAT 已失效，请重新添加该账号"}
			}
			return "", "", 0, derr
		}
		token = text(data, "token")
		refresh = text(data, "refresh_token")
		if token == "" || strings.Contains(refresh, "|") {
			return "", "", 0, &Error{Kind: ErrServer, Status: 502, Msg: "Qoder PAT 响应缺少有效 token，旧凭证未被覆盖"}
		}
		if ms := millisOf(data["expires_at"]); ms > 0 {
			exp = ms
		}
		return token, auth.PackPATRefresh(pat, refresh, a.UID, a.MachineIDValue()), exp, nil
	}

	oauth := a.OAuthRefresh()
	if oauth == "" {
		return "", "", 0, &Error{Kind: ErrClient, Status: 400, Msg: "Qoder 账号没有刷新凭证，请重新登录或添加 PAT"}
	}
	path := deviceRefreshPath
	if strings.HasPrefix(oauth, "jrt-") {
		path = jobRefreshPath
	}
	resp, err = c.doJSON(http.MethodPost, c.apiBase(realm)+path,
		map[string]any{"refresh_token": oauth}, RefreshHeaders())
	if err != nil {
		return "", "", 0, err
	}
	if resp.Status == 401 || resp.Status == 403 {
		return "", "", 0, &Error{Kind: ErrSessionDead, Status: 401, Msg: "Qoder 刷新令牌已失效或与所选地区不符，请重新登录该账号"}
	}
	data, err := payloadOf(resp, "凭证续期")
	if err != nil {
		return "", "", 0, err
	}
	if returned := text(data, "user_id", "uid", "userId"); returned != "" && a.UID != "" && returned != a.UID {
		return "", "", 0, &Error{Kind: ErrServer, Status: 502, Msg: "Qoder 续期返回了不同账号，旧凭证未被覆盖"}
	}
	token = text(data, "device_token", "deviceToken", "token", "access_token")
	if token == "" {
		return "", "", 0, &Error{Kind: ErrServer, Status: 502, Msg: "Qoder 续期响应缺少令牌，旧凭证未被覆盖"}
	}
	refresh = text(data, "refresh_token", "refreshToken")
	if strings.Contains(refresh, "|") {
		return "", "", 0, &Error{Kind: ErrServer, Status: 502, Msg: "Qoder 续期响应的 refresh_token 格式无效"}
	}
	if refresh == "" {
		refresh = oauth
	}
	for _, key := range []string{"expires_at", "expiresAt", "expire_time", "expireTime"} {
		if ms := millisOf(data[key]); ms > 0 {
			exp = ms
			break
		}
	}
	if exp == 0 {
		exp = time.Now().Add(30 * 24 * time.Hour).UnixMilli()
	}
	return token, auth.PackRefresh(refresh, a.UID, a.MachineIDValue()), exp, nil
}

// ---------------------------------------------------------------------------
// 用户资料

// GetUserInfo 拉 userinfo：uid / 昵称 / 企业标识（Qoder 无企业字段，恒空）。
func (c *Client) GetUserInfo(a *auth.Auth) (uid, nickname, enterpriseID string, err error) {
	resp, err := c.doJSON(http.MethodGet, c.apiBase(a.Realm())+UserInfoPath, nil, OpenAPIHeaders(a.JWT()))
	if err != nil {
		return "", "", "", err
	}
	data, err := payloadOf(resp, "用户信息查询")
	if err != nil {
		return "", "", "", err
	}
	return text(data, "id", "userId", "user_id"), truncate(text(data, "name", "username", "nickname"), 100), "", nil
}

// ---------------------------------------------------------------------------
// 推理

// ChatStream 把 OpenAI 请求体翻成 Qoder agent_chat_generation 并发出。
//
// 返回上游原始 SSE 响应体（信封未拆），由 Stream/Aggregate 消费；
// 非 2xx 时返回分类后的 *Error，body 片段一并带回供日志用。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	plan, err := c.buildChatPlan(a, body)
	if err != nil {
		if e, ok := err.(*Error); ok {
			return nil, e.Status, nil, err
		}
		return nil, 502, nil, &Error{Kind: ErrServer, Status: 502, Msg: err.Error()}
	}
	candidates := plan.urls
	if len(candidates) == 0 {
		candidates = []string{plan.url}
	}
	var (
		lastStatus int
		lastBody   []byte
		lastErr    error
	)
	for i, base := range candidates {
		rc, status, raw, err := c.doChat(base, plan)
		if err == nil {
			return rc, status, raw, nil
		}
		// 保留更有信息量的那个错误：端点互备时「某个备用端点连不上」不该覆盖
		// 「上游说排队」——后者才决定调用方要不要等，也决定别把号记成故障。
		if lastErr == nil || errorRank(err) > errorRank(lastErr) {
			lastErr = err
		}
		lastStatus, lastBody = status, raw
		// 只有「排队/容量窗口」和传输层失败才换端点：额度不足、凭证失效换端点也没用。
		if i == len(candidates)-1 || !endpointRetryable(err) {
			break
		}
		log.Printf("端点 %s 排队/不可达（%v），换下一个候选端点重发", base, err)
	}
	return nil, lastStatus, lastBody, lastErr
}

// doChat 向一个端点发一次已签名的推理请求。
func (c *Client) doChat(base string, plan *chatPlan) (io.ReadCloser, int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, ChatURL(base), bytes.NewReader(plan.body))
	if err != nil {
		return nil, 500, nil, &Error{Kind: ErrServer, Status: 500, Msg: "构造 Qoder 上游请求失败"}
	}
	for key, value := range plan.headers {
		req.Header.Set(key, value)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", c.currentUA())
	// 实测 chat 请求必带这两个头：上游按 X-Model-Key 路由模型档位，
	// 缺失时会被当成 Auto 处理（表现为「发出去但没按选的模型回」）。
	req.Header.Set("X-Model-Key", plan.model.UpstreamKey)
	if source := plan.model.Source; source != "" {
		req.Header.Set("X-Model-Source", source)
	} else {
		req.Header.Set("X-Model-Source", "system")
	}

	c.timeoutMu.RLock()
	resp, err := c.streamClient().Do(req)
	c.timeoutMu.RUnlock()
	if err != nil {
		return nil, 502, nil, &Error{Kind: ErrServer, Status: 502, Msg: "无法连接 Qoder，请检查网络或代理设置"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		msg := truncate(strings.TrimSpace(string(raw)), 300)
		if msg == "" {
			msg = fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode)
		}
		return nil, resp.StatusCode, raw, &Error{Kind: kind, Status: resp.StatusCode, Msg: msg,
			RetryAfterSecs: retryAfterOf(string(raw)), ServiceUnavailable: queueUnavailable(string(raw))}
	}

	// 排队还有第二种形态：HTTP 200，但流里第一帧就是 statusCodeValue=403 的信封。
	// 只看状态码会把它当成功，端点也就没法换——所以在这里窥一帧。
	br := bufio.NewReaderSize(resp.Body, 64*1024)
	first, err := peekFirstFrame(br)
	if err != nil {
		_ = resp.Body.Close()
		return nil, 502, nil, &Error{Kind: ErrServer, Status: 502, Msg: "读取上游首帧失败"}
	}
	if first.Kind == sseError {
		_ = resp.Body.Close()
		kind := first.Class
		if kind != ErrBusy && kind != ErrPlanLimit && kind != ErrSoftRate {
			kind = Classify(first.Status, first.Raw)
		}
		return nil, first.Status, []byte(first.Raw), &Error{Kind: kind, Status: first.Status,
			Msg: orDefault(first.Message, first.Raw), RetryAfterSecs: first.RetryAfterSecs,
			ServiceUnavailable: first.ServiceUnavailable}
	}
	return &peekedBody{Reader: io.MultiReader(bytes.NewReader(first.replay), br), Closer: resp.Body}, resp.StatusCode, nil, nil
}

// peekedBody 把「已经读掉的首帧」拼回流的开头。
type peekedBody struct {
	io.Reader
	io.Closer
}

// peekFirstFrame 读到第一条 data: 帧为止（最多 8 行 / 64KB），把它解析成事件，
// 同时记住原始字节以便回放——不能丢帧，否则客户端会少一段正文。
func peekFirstFrame(br *bufio.Reader) (sseEvent, error) {
	var replay bytes.Buffer
	for i := 0; i < 8; i++ {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			replay.Write(line)
			if ev := parseSSELine(strings.TrimPrefix(strings.TrimRight(string(line), "\r\n"), "data:")); ev.Kind != sseSkip {
				ev.replay = replay.Bytes()
				return ev, nil
			}
		}
		if err != nil {
			return sseEvent{Kind: sseSkip, replay: replay.Bytes()}, nil
		}
	}
	return sseEvent{Kind: sseSkip, replay: replay.Bytes()}, nil
}

// chatPlan 是一次已签名的出站请求。
type chatPlan struct {
	url     string   // 主端点（签名用它的路径，各候选路径相同）
	urls    []string // 候选端点，排队时依次换
	headers map[string]string
	body    []byte
	model   ModelInfo
}

// buildChatPlan 解析入站体 → 定型 → 编码 → 签名。
func (c *Client) buildChatPlan(a *auth.Auth, body []byte) (*chatPlan, error) {
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, &Error{Kind: ErrClient, Status: 400, Msg: "请求体不是合法 JSON"}
	}
	modelName, _ := in["model"].(string)
	if strings.TrimSpace(modelName) == "" {
		modelName = DefaultConfigName
	}
	model, ok := c.ResolveModel(modelName, a.Realm())
	if !ok {
		return nil, &Error{Kind: ErrClient, Status: 400, Msg: fmt.Sprintf("未知模型 %q：请从 /v1/models 选择", modelName)}
	}
	raw, err := json.Marshal(buildUpstreamBody(in, model, a.UID))
	if err != nil {
		return nil, &Error{Kind: ErrServer, Status: 500, Msg: "构造 Qoder 请求体失败"}
	}
	encoded := encodeBody(raw)
	url := ChatURL(c.inferenceBase(a.Realm(), a.JWT()))
	headers, err := buildAuthHeaders(encoded, url, a)
	if err != nil {
		return nil, &Error{Kind: ErrServer, Status: 500, Msg: err.Error()}
	}
	bases := c.chatBases(a.Realm(), a.JWT())
	if len(c.chatBasesOverride) > 0 {
		bases = c.chatBasesOverride
	}
	return &chatPlan{url: url, urls: bases, headers: headers, body: encoded, model: model}, nil
}

// errorRank 给错误排个「信息量」：排队 > 服务端/传输 > 其它。
func errorRank(err error) int {
	var ue *Error
	if !errors.As(err, &ue) {
		return 0
	}
	switch ue.Kind {
	case ErrBusy:
		return 3
	case ErrPlanLimit, ErrSoftRate:
		return 2
	case ErrServer:
		return 1
	}
	return 0
}

// endpointRetryable 判断这次失败该不该换端点重发：排队/服务不可用/连不上都值得换，
// 额度与凭证问题换了也一样。
func endpointRetryable(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) {
		return false
	}
	return ue.Kind == ErrBusy || ue.Kind == ErrServer
}

// retryAfterSecsRE 抓上游建议的重试间隔。正文是三层嵌套的 JSON 字符串
// （{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{...retryAfterSeconds:30}"}"}），
// 所以先去掉转义再当文本找，比逐层 Unmarshal 少一半代码。
var (
	retryAfterRE   = regexp.MustCompile(`"retryAfterSeconds"\s*:\s*(\d+)`)
	waitTimeRE     = regexp.MustCompile(`"waitTime"\s*:\s*(\d+)`)
	serviceAvailRE = regexp.MustCompile(`"serviceAvailable"\s*:\s*false`)
)

// queueUnavailable 看排队体里的 serviceAvailable:false——上游自己说「此刻不可服务」，
// 那再等建议的 30 秒也排不上，应当立刻如实回错而不是让客户端干等。
func queueUnavailable(raw string) bool {
	flat := strings.ReplaceAll(raw, `\`, "")
	return serviceAvailRE.MatchString(flat)
}

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

// PrepareBody 是入站体的轻量规整钩子：Qoder 的请求体在 buildChatPlan 里整体重建，
// 这里原样返回（保留函数以对齐面板调用点）。
func PrepareBody(src []byte) []byte { return src }

// md5Hex 供请求指纹使用。
func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
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
