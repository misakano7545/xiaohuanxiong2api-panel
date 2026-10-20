// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/qoder2api-panel/internal/auth"
	"github.com/misakano7545/qoder2api-panel/internal/pool"
	"github.com/misakano7545/qoder2api-panel/internal/prompt"
	"github.com/misakano7545/qoder2api-panel/internal/reqlog"
	"github.com/misakano7545/qoder2api-panel/internal/session"
	"github.com/misakano7545/qoder2api-panel/internal/upstream"
	"github.com/misakano7545/qoder2api-panel/internal/usage"
)

// Config handler 依赖。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	APIKey       string          // 空 = 不鉴权
	MaxRotate    int             // 单请求最多换号次数，默认 3
	RefreshSkew  time.Duration   // token 预刷新窗口，默认 10m
	DefaultModel string          // 默认 Auto
	PromptMode   string          // passthrough / custom / append
	PromptText   string          // custom/append 用的提示词文本
	Session      *session.Router // 可选，会话粘性（同一会话粘同一账号）
	Panel        http.Handler    // 可选，/panel/
	Usage        *usage.Recorder // 可选，逐请求用量台账（按 账号×模型×时间片 分桶）
	// RequestLog 可选，请求级指标/归档（nil = 不记录）。RequestClientInfo 决定事件里
	// 是否填调用来源（IP/UA）——比 token 计数敏感，运营可自行决定是否落盘。
	RequestLog        *reqlog.Recorder
	RequestClientInfo bool
	// 冷却/熔断/在途等池参数不在这里：它们在 pool.Limits（pool.ApplyLimits 热改），
	// 由 pool 自己持有，避免"参数在 handler、执行在 pool"的两处漂移。
}

// maxBodyBytes 请求体大小上限（8MB），超过返回 413。
const maxBodyBytes = 8 << 20

// errBodyTooLarge 请求体超过 maxBodyBytes。
var errBodyTooLarge = errors.New("request body too large")

// readCappedBody 读请求体并限长：三套入站协议共用同一口限（超限返回 errBodyTooLarge）。
// 用 LimitReader 在**读取前**就截断——早期 /v1/responses、/v1/messages 直接 io.ReadAll，
// 原始超大体先整个进内存才轮到检查，是持 key 即可触发的内存 DoS。
func readCappedBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBodyBytes {
		return nil, errBodyTooLarge
	}
	return body, nil
}

// Handler 主路由。
type Handler struct {
	cfg  Config
	mux  *http.ServeMux
	rtMu sync.RWMutex
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.RefreshSkew <= 0 {
		// Qoder 的 access token 寿命约 24 小时：提前 10 分钟续期即可。
		// （旧值 24h 在 Qoder 上会让每个请求都触发一次续期。）
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.DefaultModel == "" {
		cfg.DefaultModel = upstream.DefaultConfigName
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(h.responses))
	h.mux.HandleFunc("POST /v1/messages", h.withAnthropicAuth(h.messages))
	h.mux.HandleFunc("POST /messages", h.withAnthropicAuth(h.messages))
	// Claude Code 会先问上下文占多少 token 再决定要不要压缩；上游没有计数接口，本地估算。
	h.mux.HandleFunc("POST /v1/messages/count_tokens", h.withAnthropicAuth(h.countTokens))
	h.mux.HandleFunc("POST /messages/count_tokens", h.withAnthropicAuth(h.countTokens))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	if cfg.Panel != nil {
		mountPanel(h.mux, cfg.Panel)
	}
	return h
}

func mountPanel(mux *http.ServeMux, p http.Handler) {
	mux.Handle("/panel/", p)
	mux.HandleFunc("/panel", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/panel/", http.StatusPermanentRedirect)
	})
}

// MountPanel 在 NewHandler 之后挂上面板。
func (h *Handler) MountPanel(p http.Handler) {
	if h != nil && p != nil {
		mountPanel(h.mux, p)
	}
}

// Runtime handler 侧可热改的设置（池参数在 pool 里，见 Config 注释）。
type Runtime struct {
	DefaultModel string
	PromptMode   string
	PromptText   string
	// APIKey 空 = 不鉴权。整份 Runtime 一起套用，所以"面板里把密钥清空"也能立即生效。
	APIKey string
}

// ApplyRuntime 立即改默认模型、出站提示词改写与 API 密钥（面板保存配置时调用）。
func (h *Handler) ApplyRuntime(rt Runtime) {
	h.rtMu.Lock()
	defer h.rtMu.Unlock()
	if rt.DefaultModel != "" {
		h.cfg.DefaultModel = rt.DefaultModel
	}
	h.cfg.PromptMode, h.cfg.PromptText = rt.PromptMode, rt.PromptText
	h.cfg.APIKey = rt.APIKey
}

func (h *Handler) runtime() Runtime {
	h.rtMu.RLock()
	defer h.rtMu.RUnlock()
	return Runtime{
		DefaultModel: h.cfg.DefaultModel,
		PromptMode:   h.cfg.PromptMode,
		PromptText:   h.cfg.PromptText,
		APIKey:       h.cfg.APIKey,
	}
}

// applyPrompt 出站前按模式改写系统提示词；passthrough（默认）原样返回。
func (h *Handler) applyPrompt(body []byte) []byte {
	rt := h.runtime()
	switch rt.PromptMode {
	case "custom":
		return prompt.Rewrite(body, rt.PromptText)
	case "append":
		return prompt.Append(body, rt.PromptText)
	}
	return body
}

// authOK 校验 Bearer 密钥；未设密钥（want 空）= 放行（仅本机）。
// 各入站协议的 withAuth 包装只负责把失败写成自己协议形状的错误体，比较逻辑只此一份。
func (h *Handler) authOK(r *http.Request) bool {
	// 密钥随配置热改（面板改了立刻用新的），所以每次都从 runtime 读，不缓存到局部闭包。
	want := h.runtime().APIKey
	if want == "" {
		return true
	}
	authz := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(authz) < len(prefix) || !strings.EqualFold(authz[:len(prefix)], prefix) {
		return false
	}
	// 常量时间比较，防时序攻击（本地代理但按规范）。
	return subtle.ConstantTimeCompare([]byte(authz[len(prefix):]), []byte(want)) == 1
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.authOK(r) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": h.cfg.Pool.List(),
	})
}

// ---------------------------------------------------------------------------
// 模型映射
// ---------------------------------------------------------------------------

// mapModel 把客户端传入的 model 映射成上游目录里的规范 id：
//
//	"Qwen3.8-Max" / "qwen3.8-max" / "qmodel_38max" → "Qwen3.8-Max"
//	"" / "auto"                                    → 默认模型
//	其他未知                                        → 400
func (h *Handler) mapModel(model, realm string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" || strings.EqualFold(model, "auto") {
		model = h.runtime().DefaultModel
	}
	mi, ok := h.cfg.Upstream.ResolveModel(model, realm)
	if !ok {
		return "", fmt.Errorf("unknown model %q", model)
	}
	return mi.ID, nil
}

// modelRealm 返回模型归属地区（目录里没有就返回空）。
func (h *Handler) modelRealm(model string) string {
	if mi, ok := h.cfg.Upstream.ResolveModel(model, auth.RealmGlobal); ok {
		return mi.Realm
	}
	return ""
}

// modelList 是 /v1/models 的内容：官方目录两区各自成条，id 带 `realm:` 前缀。
// 同名模型两区下发的 id 字符串一模一样，不前缀就只能留一条；前缀即 resolve_model.go
// 里的路由协议，客户端挑哪个地区就发哪个前缀，裸名仍按账号地区兜底。
func (h *Handler) modelList() []map[string]any {
	out := make([]map[string]any, 0, 32)
	for _, mi := range h.cfg.Upstream.Catalog() {
		out = append(out, map[string]any{
			"id":                mi.Realm + ":" + mi.ID,
			"object":            "model",
			"created":           1753600000,
			"owned_by":          "qoder-" + mi.Realm,
			"context_length":    mi.ContextWindow,
			"max_output_tokens": mi.MaxOutput,
			"reasoning":         mi.Reasoning,
			"efforts":           mi.Efforts,
			"vision":            mi.VL,
			"credits":           mi.Credits,
		})
	}
	return out
}

// Models 是面板模型页的载荷：每条带 realm 前缀与页面需要的展示字段（能力胶囊、
// 思考档位、倍率、上下文）。与 /v1/models 分开，避免把面板字段混进 OpenAI 契约。
func (h *Handler) Models() []map[string]any {
	catalog := h.cfg.Upstream.Catalog()
	out := make([]map[string]any, 0, len(catalog))
	for _, mi := range catalog {
		entry := map[string]any{
			"id":                      mi.Realm + ":" + mi.ID,
			"name":                    mi.Name,
			"object":                  "model",
			"context_length":          mi.ContextWindow,
			"max_output_tokens":       mi.MaxOutput,
			"function":                mi.Source,
			"supports_image":          mi.VL,
			"reasoning_effort_config": strings.Join(mi.Efforts, "/"),
			"upstream_key":            mi.UpstreamKey,
			"credits":                 mi.Credits,
			"thinking":                map[bool]string{true: "enabled", false: "disabled"}[mi.Reasoning],
		}
		if mi.Reasoning {
			entry["capability"] = "reasoning_model"
		}
		var factor float64
		if _, err := fmt.Sscanf(mi.Credits, "x%g", &factor); err == nil {
			entry["credit_rate"] = factor
		}
		out = append(out, entry)
	}
	return out
}

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// setModelInBody 把出站 model 定成规范 id（其余字段原样保留）。
func setModelInBody(body []byte, modelID string) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	obj["model"] = modelID
	raw, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return raw
}

// chatRequest 是入站体的窥探结构（只取路由需要的字段，其余由上游客户端自己解析）。
type chatRequest struct {
	Stream bool   `json:"stream"`
	Model  string `json:"model"`
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := readCappedBody(r)
	if errors.Is(err, errBodyTooLarge) {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 8MB limit")
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek chatRequest
	_ = json.Unmarshal(body, &peek)

	// 地区前缀（[realm:]model）在这里剥掉：后面 mapModel / 出站 body / 台账都用裸名。
	realm, bareModel := resolveModel(peek.Model)
	// 请求记录 = 最终地区 + 客户端要的名字（-max 后缀下面会被剥掉，先留一份）。realm 要等
	// 两次兜底才定死，所以闭包复写：mapModel 报 400 提前返回时也要带上名字。
	shownModel := bareModel
	setTraceModel := func() {
		if tr := traceFrom(r); tr != nil {
			tr.Model = realm + ":" + shownModel
		}
	}
	prefixed := bareModel != peek.Model
	if !prefixed {
		// 裸名：按模型表里该名字的归属地区兜底（表里没有 = 国内版，老客户端零回归）。
		if r := h.modelRealm(strings.TrimSpace(bareModel)); r != "" {
			realm = r
		}
	}

	// 会话键必须在改写提示词之前取：改写会动 messages，之后取会让键漂移。
	sessKey := session.ExtractKey(body)

	bareModel = strings.TrimSpace(bareModel)

	configName, err := h.mapModel(bareModel, realm)
	if err != nil {
		setTraceModel()
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// 裸名经别名落到别的地区（如 Gemini-3.1-Pro-Preview → gemini-3.1-pro）：地区以模型为准，
	// 否则会拿国内号去接国际模型。带前缀的请求_prefix 权威，不动。
	if !prefixed {
		if r := h.modelRealm(configName); r != "" {
			realm = r
		}
	}

	// 台账用**解析后的**上游模型名（config_name）：客户端可能发 cn:glm-5.2 或别名，
	// 按入站原样分桶会把同一个模型在面板上拆成多行，别名还查不到单价（积分显示 —）。
	// 请求记录（tr.Model）记 realm + 客户端裸名 —— 面板上直接看得出这次走的是国内还是国际，
	// 两边口径不同是故意的。
	setTraceModel()
	peek.Model = configName

	body = setModelInBody(body, configName)
	body = h.applyPrompt(body)

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 按地区选号：国际版模型只有国际号能做，反之亦然。
		acct, release := h.acquireAccount(tried, sessKey, realm)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		if tr := traceFrom(r); tr != nil && tr.Account == "" {
			tr.Account = accountLabel(acct.UID, acct.Nickname)
		}
		// defer 而不是顺序调用：attempt 内任何 panic（上游客户端、续写器）都会跳过顺序
		// 调用，把这一号的在途名额永久漏掉；闭包让每个号各 defer 一次。
		var done bool
		var err error
		// 排队（10605 / isQueued）不是失败，也不该换号：它是模型侧的窗口。按上游建议的
		// 时长等一会儿，对**同一账号**重发同一请求（最多 2 次）；客户端断开就不再白打。
		for retry := 0; ; retry++ {
			func() {
				defer release()
				done, err = h.attempt(r.Context(), w, acct, body, peek, sessKey, traceFrom(r))
			}()
			if done || err == nil || !isQueueError(err) || queueHopeless(err) || retry >= 2 {
				break
			}
			delay := queueDelay(err)
			log.Printf("排队 %s：模型 %s 暂不可服务，%s 后重发（第 %d/2 次）", acct.UID, peek.Model, delay, retry+1)
			select {
			case <-r.Context().Done():
				return
			case <-time.After(delay):
			}
		}
		if done {
			return
		}
		if err != nil {
			// 调用方参数错（4xx，与账号无关，同 4001/4011 那一类）：如实回上游状态码，
			// 不再换号——换号只会把每个号都罚一次降权，最后还谎报成「号都不可用」。
			var ue *upstream.Error
			if errors.As(err, &ue) && (ue.Kind == upstream.ErrClient || ue.Kind == upstream.ErrNone || ue.Kind == upstream.ErrGateway) {
				writeOpenAIError(w, ue.Status, "upstream_rejected", ue.Msg)
				return
			}
			lastErr = err
		}
	}
	// 上游明确说是「这个模型/通道不行」（4001 参数、4011 通道限流…，Kind()==ErrNone）时，
	// 别再报「账号都不可用」——号是好的，是请求本身被上游拒了。照实回上游的原话。
	// 实测 2026-09-28：coder 通道 4011 期间，同一账号的 Work 通道照常出正文。
	if lastErr != nil {
		if isQueueError(lastErr) {
			msg := "上游模型排队中（" + peek.Model + "）：已按上游建议重试 2 次仍排不上，请稍后再发"
			if queueHopeless(lastErr) {
				msg = "上游当前对该模型不可服务（" + peek.Model + "，serviceAvailable=false）：这是上游容量/套餐侧状态，与账号无关，请稍后再发或换模型"
			}
			writeOpenAIError(w, http.StatusServiceUnavailable, "qoder_model_busy", msg)
			return
		}
		var se *upstream.StreamError
		if errors.As(lastErr, &se) && se.Kind() == upstream.ErrNone {
			writeOpenAIError(w, http.StatusTooManyRequests, "upstream_rejected", se.Error())
			return
		}
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// isQueueError 判断是不是「上游排队」：这既不罚号，也不该立刻回错。
func isQueueError(err error) bool {
	// 排队有两种落点：HTTP 403 直接拒（*Error），或 200 信封里 statusCodeValue=403（*StreamError）。
	var ue *upstream.Error
	if errors.As(err, &ue) && ue.Kind == upstream.ErrBusy {
		return true
	}
	var se *upstream.StreamError
	return errors.As(err, &se) && se.Kind() == upstream.ErrBusy
}

// queueHopeless 上游明说 serviceAvailable:false 时不再等：等满建议时长也排不上。
func queueHopeless(err error) bool {
	var ue *upstream.Error
	if errors.As(err, &ue) && ue.ServiceUnavailable {
		return true
	}
	var se *upstream.StreamError
	return errors.As(err, &se) && se.ServiceUnavailable
}

// queueDelay 跟随上游建议的重试间隔（retryAfterSeconds / waitTime），缺省 15 秒，
// 上限 30 秒——等得比上游建议更久只会把首字等待拖长。
func queueDelay(err error) time.Duration {
	secs := int64(15)
	var ue *upstream.Error
	if errors.As(err, &ue) && ue.RetryAfterSecs > 0 {
		secs = ue.RetryAfterSecs
	}
	var se *upstream.StreamError
	if errors.As(err, &se) && se.RetryAfterSecs > 0 {
		secs = se.RetryAfterSecs
	}
	if secs > 30 {
		secs = 30
	}
	return time.Duration(secs) * time.Second
}

// acquireAccount 挑一个账号并占用在途名额：会话粘性命中优先，否则按权重挑。
//
// 返回的 release 必须在这次尝试结束（无论成败）后调用一次——在途名额泄漏会把账号
// 慢慢挤出池（达到 max_in_flight 后不再可选）。
func (h *Handler) acquireAccount(tried map[string]bool, sessKey, realm string) (*auth.Auth, func()) {
	if sessKey != "" && h.cfg.Session != nil {
		if uid, ok := h.cfg.Session.Resolve(sessKey); ok && !tried[uid] {
			if a, ok := h.cfg.Pool.AcquireIfHealthy(uid); ok {
				if realm == "" || a.Realm() == realm {
					return a, func() { h.cfg.Pool.Release(uid) }
				}
				// 粘住的号地区不符（换模型/换地区了）→ 还回在途名额再解绑，别白占着。
				h.cfg.Pool.Release(uid)
			}
			// 粘住的号不可用（冷却/禁用/在途满/地区不符）→ 解绑，本轮重新分配。
			h.cfg.Session.Unbind(sessKey)
		}
	}
	a := h.cfg.Pool.PickRealmExcluding(realm, tried)
	if a == nil {
		return nil, func() {}
	}
	uid := a.UID
	return a, func() { h.cfg.Pool.Release(uid) }
}

// attempt 对一个账号跑一次完整尝试：预刷新 token → 出站 → 分类记账 → 写响应。
//
// done=true 表示响应已写给客户端，调用方必须停止轮转；err 非 nil 表示这次尝试失败、
// 可以换下一个号（错误只用于最终 503 的说明文案）。
func (h *Handler) attempt(ctx context.Context, w http.ResponseWriter, acct *auth.Auth, body []byte, peek chatRequest, sessKey string, tr *reqTrace) (bool, error) {
	refreshed, err := h.cfg.Upstream.RefreshTokenIfNeeded(acct, h.cfg.RefreshSkew)
	if err != nil {
		var ue *upstream.Error
		if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
			// 一次 401 不杀号：连续 NoteSessionDead 达阈值才禁用（阈值见 pool）。
			h.cfg.Pool.NoteSessionDead(acct.UID)
		} else {
			// token 刷新失败不是一次对话调用，不计用量；但账号本身有问题，按罚号记一次。
			h.cfg.Pool.NoteError(acct.UID)
		}
		return false, err
	}
	if refreshed {
		if err := acct.SaveAtomic(); err != nil {
			// refreshToken 每次换票都**轮换**：新值只有落盘成功才算真到手。写失败就只留在
			// 内存里，重启后拿旧 token 去换 → 上游判失效 → 账号被自动禁用，日志里查不到原因。
			log.Printf("token 落盘失败 uid=%s（重启后需重新登录该账号）: %v", acct.UID, err)
		}
		// 刷新成功 = 号还活着，清连续失效计数（没有真刷新就不清，否则每请求清零，
		// 连续计数永远到不了禁用阈值）。
		h.cfg.Pool.ClearSessionDead(acct.UID)
	}

	attemptStart := time.Now()
	rc, status, respBody, terr := h.cfg.Upstream.ChatStream(acct, body)
	if terr != nil {
		// 出站失败也要分类：排队/额度不是账号故障，无脑 NoteError 会把好号推进熔断
		// （实测：上游一整天 p3 排队，账号被自己的排队记成 breaker）。
		h.noteUsage(acct.UID, peek.Model, attemptStart, false, nil, tr)
		kind := upstream.ErrServer
		var ue *upstream.Error
		if errors.As(terr, &ue) {
			kind = ue.Kind
		}
		h.noteFailure(acct.UID, kind)
		return false, terr
	}
	if status >= 400 {
		kind := upstream.Classify(status, string(respBody))
		h.noteUsage(acct.UID, peek.Model, attemptStart, false, nil, tr)
		h.noteFailure(acct.UID, kind)
		return false, &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
	}

	if peek.Stream {
		h.cfg.Pool.NoteSuccess(acct.UID)
		// 流内业务错误（额度/限流/会话失效/排队）→ 记进账号状态，错误信息注入 SSE。
		streamErr := false
		usg, serr := upstream.StreamWithError(w, rc, func(se *upstream.StreamError) {
			streamErr = true
			h.handleStreamError(acct.UID, se)
		})
		rc.Close()
		// serr 非 nil 只可能是「一个字节都没写给客户端」的失败（空流 / 首帧就是错误信封）：
		// 此时响应还没提交，必须交回调用方换号或如实回错误体——早先这里只处理空流，
		// 首帧错误的流会 return true 变成空 200（客户端收到的是一段没有内容的回复）。
		if serr != nil {
			h.noteUsage(acct.UID, peek.Model, attemptStart, false, nil, tr)
			return false, serr
		}
		// 流内 error 事件走 onErr 回调后 serr 仍是 nil，所以成败要看**两处**：返回的 err
		// 与回调有没有触发过。不看回调会把 1005 记成成功；反过来拿「有没有 token_usage」
		// 当判据又会把「正文完整但没给 usage」的回复记成失败并解绑会话粘性。
		ok := serr == nil && !streamErr
		h.noteUsage(acct.UID, peek.Model, attemptStart, ok, usg, tr)
		if ok {
			h.bindSession(sessKey, acct.UID)
		} else if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Unbind(sessKey)
		}
		return true, nil
	}

	resp, err := upstream.Aggregate(rc)
	rc.Close() // 已完全消费，立即释放上游连接（防轮转 continue 泄漏 body）
	if err != nil {
		var se *upstream.StreamError
		if errors.As(err, &se) {
			// 流内错误与 HTTP 级错误走同一张分类表（noteFailure），避免两处口径漂移。
			h.noteUsage(acct.UID, peek.Model, attemptStart, false, nil, tr)
			h.noteFailure(acct.UID, se.Kind())
			return false, err
		}
		if errors.Is(err, upstream.ErrEmptyStream) {
			// 同上：空转可换号重试，但不算账号故障（不 NoteError）。
			h.noteUsage(acct.UID, peek.Model, attemptStart, false, nil, tr)
			return false, err
		}
		h.noteUsage(acct.UID, peek.Model, attemptStart, false, nil, tr)
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
		return true, err
	}
	h.cfg.Pool.NoteSuccess(acct.UID)
	h.noteUsage(acct.UID, peek.Model, attemptStart, true, usageOf(resp), tr)
	h.bindSession(sessKey, acct.UID)
	writeJSON(w, http.StatusOK, resp)
	return true, nil
}

// noteFailure 按错误类型记账（HTTP 级与流内错误共用这一张表）：
//   - 1005 权益不足 → 计划硬冷却（plan_credit）；
//   - 429/4008 限流 → 软冷却（指数退避到 soft_rate_max）；
//   - 401 session 失效 → 连续达阈值才禁用；
//   - 5xx → 熔断计数；
//   - 其余 4xx（参数/404 等）→ 降权计数，不罚号；
//   - ErrNone（模型/参数问题，如 4001）→ 不记账：与账号无关，写进去只会污染状态。
func (h *Handler) noteFailure(uid string, kind upstream.ErrKind) {
	switch kind {
	case upstream.ErrNone:
		// no-op
	case upstream.ErrClient:
		// 调用方 4xx（参数/未识别的业务码，11101 这类）与账号无关：罚号只会把好号推向
		// 降权→熔断，而真正该做的是把上游状态码如实回给调用方（见 chatCompletions 的
		// fail-fast 分支）。
		// no-op
	case upstream.ErrPlanLimit:
		h.cfg.Pool.CooldownPlan(uid)
	case upstream.ErrSoftRate:
		h.cfg.Pool.CooldownSoft(uid, "429 rate limit")
	case upstream.ErrSessionDead:
		h.cfg.Pool.NoteSessionDead(uid)
	case upstream.ErrServer:
		h.cfg.Pool.NoteError(uid)
	case upstream.ErrBusy:
		// 排队/资源受限是模型侧的窗口，与账号健康无关：罚号只会把好号推向熔断。
		// no-op
	case upstream.ErrGateway:
		// 网关超时/请求过大：与账号健康无关，换号只会把每个号都罚一遍还白重发巨体。
		// no-op
	case upstream.ErrReasoningLoop:
		// 思考链复读是模型侧问题，换号也未必更好；不罚号，客户端自己决定要不要重试。
		// no-op
	default:
		h.cfg.Pool.NoteDegrade(uid)
	}
}

// bindSession 会话粘性绑定：只有真的产出回复才绑，失败继续留在池里轮换。
func (h *Handler) bindSession(sessKey, uid string) {
	if sessKey != "" && h.cfg.Session != nil {
		h.cfg.Session.Bind(sessKey, uid)
	}
}

// handleStreamError 流式响应中的上游业务错误 → pool 状态机。
//
// 与 HTTP 级错误同一张分类表：Kind() 现在能区分「模型/参数问题」（ErrNone，不记账）
// 与「限流/会话失效/服务端」，所以不再需要"一律按罚号计数"的粗口径。
func (h *Handler) handleStreamError(uid string, se *upstream.StreamError) {
	h.noteFailure(uid, se.Kind())
}

// noteUsage 把一次出站尝试记进用量台账。
//
// 失败尝试也要记（请求数与失败数）——重试放大正是靠这一列才在面板里看得见；
// 但「刷新 token 失败」不记：那不是一次对话调用，记进去只会让用量虚高。
// ok 以「上游是否给了 usage」为准：流内 error 事件（如 1005）响应体正常写完、
// 函数返回 nil，只有 token_usage 才证明这次真的产出了回复。
func (h *Handler) noteUsage(uid, model string, started time.Time, ok bool, upstreamUsage map[string]any, tr *reqTrace) {
	// 延迟下界 1ms：本地极快响应算 TPS 时不能除以 0。
	latencyMs := time.Since(started).Milliseconds()
	if latencyMs < 1 {
		latencyMs = 1
	}
	d := usage.Delta{LatencyMs: latencyMs, HasLatency: true}
	if pt, has := numField(upstreamUsage, "prompt_tokens"); has {
		d.PromptTokens, d.HasPromptTokens = pt, true
	}
	if ct, has := numField(upstreamUsage, "completion_tokens"); has {
		d.CompletionTokens, d.HasCompletion = ct, true
		if ct > 0 {
			d.TokensPerSecond, d.HasTPS = float64(ct)*1000/float64(latencyMs), true
		}
	}
	if tt, has := numField(upstreamUsage, "total_tokens"); has {
		d.TotalTokens, d.HasTotal = tt, true
	}
	// 缓存命中：SOLO 的 token_usage 用 cache_read_input_tokens 报命中量。
	// 上游没报这个键就是没有缓存信息（不是 0% 命中），HasCacheHit 保持 false。
	if ch, has := numField(upstreamUsage, "cache_read_input_tokens"); has {
		d.CacheHitTokens, d.HasCacheHit = ch, true
	}
	// 请求记录（面板「运行日志」页的请求表）与用量台账吃同一个 Delta：token/积分/缓存命中
	// 在这里一次性带上，避免两处各算一遍再对不上。失败尝试同样带 token（上游有时在错误
	// 响应里也回了 usage），积分则由单价估算——和台账的「积分≈」是同一套口径。
	//
	// 先喂请求记录、再写台账：台账 recorder 为 nil（未接用量）时请求记录仍要有 token，
	// 否则这两件事被隐式绑在一起——「没用量台账」会顺带让运行日志的请求行也变成 tok=-。
	tr.addUsage(model, d)
	if h.cfg.Usage == nil {
		return
	}
	h.cfg.Usage.Add(time.Now(), uid, model, d, ok)
}

// usageOf 取聚合响应里的 usage（上游没给时为 nil，不伪造）。
func usageOf(resp map[string]any) map[string]any {
	m, _ := resp["usage"].(map[string]any)
	return m
}

// numField 读 usage 里的整数字段：JSON 解出来是 float64，探针/测试里可能是 int。
func numField(m map[string]any, key string) (int64, bool) {
	if m == nil {
		return 0, false
	}
	switch v := m[key].(type) {
	case float64:
		return int64(v), true
	case int:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
