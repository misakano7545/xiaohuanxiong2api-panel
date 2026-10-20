// Package server 暴露 OpenAI / Anthropic / Responses 三套入站协议，内部驱动
// pool 挑号 + upstream 转发。三套协议最后都落到同一个 chatCompletions：
// 小浣熊上游本身就是 OpenAI chat/completions 形状，所以 /v1/chat/completions 是透传，
// 另外两套由 messages.go / responses.go 把入站请求翻成 chat、再把 chat 的 SSE 翻回去。
package server

import (
	"bufio"
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

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/pool"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/reqlog"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/upstream"
)

// Config 是 Handler 依赖。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	AuthDir      string
	APIKey       string
	MaxRotate    int
	RefreshSkew  time.Duration
	DefaultModel string
	Models       []string // /v1/models 展示与校验的白名单；空 = 用上游目录
	Log          *reqlog.Recorder
	Panel        http.Handler
	Version      string
}

// Handler 是主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux

	rtMu         sync.RWMutex
	apiKey       string
	defaultModel string

	refreshMu sync.Mutex
	refreshes map[string]*sync.Mutex

	catMu  sync.Mutex
	catAt  time.Time
	catVal []upstream.Model
}

// NewHandler 构建路由。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 1
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 5 * time.Minute
	}
	h := &Handler{
		cfg:          cfg,
		mux:          http.NewServeMux(),
		apiKey:       cfg.APIKey,
		defaultModel: cfg.DefaultModel,
		refreshes:    map[string]*sync.Mutex{},
	}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(h.responses))
	h.mux.HandleFunc("POST /v1/messages", h.withAnthropicAuth(h.messages))
	h.mux.HandleFunc("POST /messages", h.withAnthropicAuth(h.messages))
	// Claude Code 会先问一次 token 数；小浣熊没有计数接口，按字符粗估（见 countTokens）。
	h.mux.HandleFunc("POST /v1/messages/count_tokens", h.withAnthropicAuth(h.countTokens))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	if cfg.Panel != nil {
		h.mux.Handle("/panel/", cfg.Panel)
		h.mux.HandleFunc("/panel", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/panel/", http.StatusPermanentRedirect)
		})
	}
	return h
}

// ServeHTTP 实现 http.Handler。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// SetAPIKey 热改入站密钥（面板保存配置时调用）。
func (h *Handler) SetAPIKey(key string) {
	h.rtMu.Lock()
	h.apiKey = key
	h.rtMu.Unlock()
}

func (h *Handler) key() string {
	h.rtMu.RLock()
	defer h.rtMu.RUnlock()
	return h.apiKey
}

func (h *Handler) defaultModelName() string {
	h.rtMu.RLock()
	defer h.rtMu.RUnlock()
	return h.defaultModel
}

// authOK 校验 Bearer / x-api-key；密钥为空表示不鉴权。
func (h *Handler) authOK(r *http.Request) bool {
	want := h.key()
	if want == "" {
		return true
	}
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if got == "" {
		got = r.Header.Get("x-api-key")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
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

// ---------------------------------------------------------------------------
// chat/completions

type chatPeek struct {
	Model  string          `json:"model"`
	Stream bool            `json:"stream"`
	Tools  json.RawMessage `json:"tools"`
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	ev := reqlog.Event{Protocol: "chat"}
	defer func() {
		ev.DurationMs = time.Since(started).Milliseconds()
		h.record(ev)
	}()

	body, err := readCappedBody(r)
	if errors.Is(err, errBodyTooLarge) {
		ev.Status = http.StatusRequestEntityTooLarge
		writeOpenAIError(w, ev.Status, "request_too_large", "request body exceeds 8MB limit")
		return
	}
	if err != nil {
		ev.Status = http.StatusBadRequest
		writeOpenAIError(w, ev.Status, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek chatPeek
	if err := json.Unmarshal(body, &peek); err != nil {
		ev.Status = http.StatusBadRequest
		writeOpenAIError(w, ev.Status, "invalid_request", "invalid JSON: "+err.Error())
		return
	}
	clientModel := strings.TrimSpace(peek.Model)
	if clientModel == "" {
		clientModel = h.defaultModelName()
	}
	upModel, ok := h.resolveModel(r.Context(), clientModel)
	if !ok {
		ev.Model = clientModel
		ev.Status = http.StatusBadRequest
		msg := "unknown model " + clientModel + ", see /v1/models"
		writeOpenAIError(w, ev.Status, "model_not_found", msg)
		return
	}
	ev.Model = upModel
	ev.Stream = peek.Stream
	body = setModel(body, upModel)

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.Pick(tried)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		ev.Account = accountLabel(acct)
		done, err := h.attempt(r.Context(), w, acct, body, peek.Stream, &ev)
		if done {
			return
		}
		if err == nil {
			continue
		}
		lastErr = err
		var ue *upstream.Error
		if errors.As(err, &ue) && ue.Kind == upstream.KindClient {
			// 请求本身被上游拒了（参数/额度/权限）：换号无用，照实透传状态码与原话。
			ev.Status = ue.Status
			ev.Error = ue.Message
			writeOpenAIError(w, ue.Status, "upstream_rejected", ue.Message)
			return
		}
	}
	ev.Status = http.StatusServiceUnavailable
	msg := "no healthy account available"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	ev.Error = msg
	writeOpenAIError(w, ev.Status, "no_healthy_account", msg)
}

// attempt 用指定账号发一次请求。done=true 表示响应已写回客户端。
func (h *Handler) attempt(ctx context.Context, w http.ResponseWriter, acct *auth.Auth, body []byte, stream bool, ev *reqlog.Event) (bool, error) {
	release := func() { h.cfg.Pool.Release(acct.UID) }
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

	if err := h.refreshIfNeeded(ctx, acct, false); err != nil {
		h.cfg.Pool.NoteError(acct.UID)
		return false, err
	}

	rc, status, err := h.cfg.Upstream.Chat(ctx, acct.Token(), body)
	if err == nil && status >= 200 && status < 300 {
		h.cfg.Pool.NoteSuccess(acct.UID)
		if stream {
			h.streamBack(ctx, w, rc, ev)
		} else {
			h.jsonBack(w, rc, ev)
		}
		rc.Close()
		return true, nil
	}

	var ue *upstream.Error
	if err != nil && !errors.As(err, &ue) {
		// 传输层错误（超时/连接断）：不是账号的错，但也换号重试。
		h.cfg.Pool.NoteError(acct.UID)
		return false, err
	}
	if err == nil {
		err = &upstream.Error{Kind: upstream.KindUnknown, Status: status}
	}
	if !errors.As(err, &ue) {
		return false, err
	}

	// 401/403：强制刷新同号再试一次（token 过期是最常见的原因，换号前先救一次）。
	if ue.Kind == upstream.KindAuth && acct.RefreshValue() != "" {
		if rerr := h.refreshIfNeeded(ctx, acct, true); rerr == nil {
			if rc2, status2, err2 := h.cfg.Upstream.Chat(ctx, acct.Token(), body); err2 == nil && status2 >= 200 && status2 < 300 {
				h.cfg.Pool.NoteSuccess(acct.UID)
				if stream {
					h.streamBack(ctx, w, rc2, ev)
				} else {
					h.jsonBack(w, rc2, ev)
				}
				rc2.Close()
				return true, nil
			}
		}
		h.cfg.Pool.NoteError(acct.UID)
		return false, ue
	}
	switch ue.Kind {
	case upstream.KindRate:
		h.cfg.Pool.Cooldown(acct.UID, h.cooldown(), "上游限流 "+ue.Message)
	case upstream.KindServer:
		h.cfg.Pool.NoteError(acct.UID)
	}
	return false, ue
}

func (h *Handler) cooldown() time.Duration { return h.cfg.Pool.CooldownDuration() }

// refreshIfNeeded 续期 token（force=true 时无条件刷）。同号并发只刷一次。
func (h *Handler) refreshIfNeeded(ctx context.Context, acct *auth.Auth, force bool) error {
	if acct.RefreshValue() == "" {
		return nil
	}
	if !force && !acct.NeedsRefresh(h.cfg.RefreshSkew) {
		return nil
	}
	h.refreshMu.Lock()
	mu, ok := h.refreshes[acct.UID]
	if !ok {
		mu = &sync.Mutex{}
		h.refreshes[acct.UID] = mu
	}
	h.refreshMu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	// 拿到锁后再判一次：等锁期间别的请求可能已经刷好了。
	if !force && !acct.NeedsRefresh(h.cfg.RefreshSkew) {
		return nil
	}
	access, refresh, err := h.cfg.Upstream.Refresh(ctx, acct.RefreshValue())
	if err != nil {
		return err
	}
	acct.ApplyTokens(access, refresh)
	if saveErr := acct.SaveAtomic(h.cfg.AuthDir); saveErr != nil {
		// 新 refresh_token 已轮换，写盘失败只警告：内存里还能用，重启后需重新登录。
		log.Printf("token 落盘失败 uid=%s: %v", acct.UID, saveErr)
	}
	return nil
}

// streamBack 把上游 SSE 原样透传，只改写每帧的 model 名并顺手记 usage。
func (h *Handler) streamBack(ctx context.Context, w http.ResponseWriter, rc io.ReadCloser, ev *reqlog.Event) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	ev.Status = http.StatusOK
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(rc)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			h.writeSSELine(w, flusher, line, ev)
		}
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				ev.Error = "upstream stream interrupted: " + err.Error()
			}
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// writeSSELine 处理一行：data 帧改写 model、记 usage，其余原样。
func (h *Handler) writeSSELine(w io.Writer, flusher http.Flusher, line string, ev *reqlog.Event) {
	trimmed := strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(trimmed, "data:") {
		_, _ = io.WriteString(w, line)
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload != "" && payload != "[DONE]" {
		var frame map[string]json.RawMessage
		if json.Unmarshal([]byte(payload), &frame) == nil {
			if raw, ok := frame["usage"]; ok {
				applyUsage(ev, raw)
			}
			if modelJSON, err := json.Marshal(ev.Model); err == nil {
				frame["model"] = modelJSON
			}
			if rewritten, err := json.Marshal(frame); err == nil {
				_, _ = io.WriteString(w, "data: "+string(rewritten)+"\n")
				if flusher != nil {
					flusher.Flush()
				}
				return
			}
		}
	}
	_, _ = io.WriteString(w, line)
	if flusher != nil {
		flusher.Flush()
	}
}

// jsonBack 把非流式响应读出来，改写 model 后下发。
func (h *Handler) jsonBack(w http.ResponseWriter, rc io.ReadCloser, ev *reqlog.Event) {
	data, err := io.ReadAll(io.LimitReader(rc, 32<<20))
	if err != nil {
		ev.Status = http.StatusBadGateway
		ev.Error = "read upstream response: " + err.Error()
		writeOpenAIError(w, ev.Status, "upstream_read_error", ev.Error)
		return
	}
	var frame map[string]json.RawMessage
	if json.Unmarshal(data, &frame) != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		ev.Status = http.StatusOK
		_, _ = w.Write(data)
		return
	}
	if raw, ok := frame["usage"]; ok {
		applyUsage(ev, raw)
	}
	if modelJSON, err := json.Marshal(ev.Model); err == nil {
		frame["model"] = modelJSON
	}
	out, err := json.Marshal(frame)
	if err != nil {
		out = data
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	ev.Status = http.StatusOK
	_, _ = w.Write(out)
}

func applyUsage(ev *reqlog.Event, raw json.RawMessage) {
	var u struct {
		PromptTokens            int64 `json:"prompt_tokens"`
		CompletionTokens        int64 `json:"completion_tokens"`
		CompletionTokensDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return
	}
	ev.Prompt, ev.Completion = u.PromptTokens, u.CompletionTokens
	ev.Reasoning = u.CompletionTokensDetails.ReasoningTokens
}

func (h *Handler) record(ev reqlog.Event) {
	if h.cfg.Log == nil {
		return
	}
	if ev.Status == 0 {
		ev.Status = http.StatusOK
	}
	h.cfg.Log.Add(ev)
}

// ---------------------------------------------------------------------------
// models / status / healthz

// resolveModel 校验并把客户端模型名映射成上游 id。返回 false 表示不认识该模型。
func (h *Handler) resolveModel(ctx context.Context, name string) (string, bool) {
	if len(h.cfg.Models) > 0 {
		for _, m := range h.cfg.Models {
			if m == name {
				return name, true
			}
		}
		return name, false
	}
	models, err := h.catalog(ctx)
	if err != nil || len(models) == 0 {
		// 目录拉不到时不拦请求：上游自己会给出准确报错，比网关猜要准。
		return name, true
	}
	for _, m := range models {
		if m.ID == name {
			return name, true
		}
	}
	return name, false
}

// catalog 拉上游目录（10 分钟缓存）。
func (h *Handler) catalog(ctx context.Context) ([]upstream.Model, error) {
	h.catMu.Lock()
	defer h.catMu.Unlock()
	if len(h.catVal) > 0 && time.Since(h.catAt) < 10*time.Minute {
		return h.catVal, nil
	}
	acct := h.cfg.Pool.Pick(nil)
	if acct == nil {
		return h.catVal, fmt.Errorf("no account")
	}
	defer h.cfg.Pool.Release(acct.UID)
	models, err := h.cfg.Upstream.Catalog(ctx, acct.Token())
	if err != nil {
		return h.catVal, err
	}
	h.catVal, h.catAt = models, time.Now()
	return models, nil
}

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	names := h.cfg.Models
	if len(names) == 0 {
		models, err := h.catalog(r.Context())
		if err != nil && len(models) == 0 {
			writeOpenAIError(w, http.StatusBadGateway, "catalog_unavailable", "上游模型目录不可用: "+err.Error())
			return
		}
		for _, m := range models {
			if m.Visible {
				names = append(names, m.ID)
			}
		}
	}
	now := time.Now().Unix()
	data := make([]map[string]any, 0, len(names))
	for _, name := range names {
		data = append(data, map[string]any{
			"id":       name,
			"object":   "model",
			"created":  now,
			"owned_by": "xiaohuanxiong",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	stats := reqlog.Stats{}
	if h.cfg.Log != nil {
		stats = h.cfg.Log.Stats()
	}
	out := map[string]any{
		"version":   h.cfg.Version,
		"accounts":  len(h.cfg.Pool.Statuses()),
		"available": h.cfg.Pool.Available(),
		"requests":  stats,
		"protocols": []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"},
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"accounts":  len(h.cfg.Pool.Statuses()),
		"available": h.cfg.Pool.Available(),
	})
}

// setModel 只替换 body 里的 model 键，其余字段原样保留。
func setModel(body []byte, model string) []byte {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return body
	}
	raw, err := json.Marshal(model)
	if err != nil {
		return body
	}
	fields["model"] = raw
	out, err := json.Marshal(fields)
	if err != nil {
		return body
	}
	return out
}

func accountLabel(a *auth.Auth) string {
	snap := a.Snapshot()
	if snap.Name != "" {
		return snap.Name + "(" + snap.UID + ")"
	}
	return snap.UID
}
