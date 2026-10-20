// models.go 模型目录：小浣熊 GET {llm_base}/model_catalog 的解析、缓存与解析口径。
//
// 上游形态 {code,data:{categories:[{type,models:[{name,description,tags,visible,
// context_window,max_tokens,billing_multiplier,params:{...}}]}]}}；
// context_window/max_tokens 可能是字符串，也可能嵌在 params 里（两个都认）。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
)

// ModelInfo 是归一化后的目录条目（面板与 /v1/models 共用的形状）。
type ModelInfo struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Realm         string   `json:"realm"`
	ContextWindow int64    `json:"context_length"`
	MaxOutput     int64    `json:"max_output_tokens"`
	Reasoning     bool     `json:"reasoning"`
	Efforts       []string `json:"efforts,omitempty"`
	VL            bool     `json:"vision"`
	Credits       string   `json:"credits,omitempty"`
	Source        string   `json:"function,omitempty"`
	UpstreamKey   string   `json:"upstream_key,omitempty"`
	Visible       bool     `json:"visible"`
}

// staticModels 是目录拉不到时的兜底（同名同 id，避免面板空表）。
func staticModels() []ModelInfo {
	return []ModelInfo{
		{ID: DefaultConfigName, Name: "Raccoon Chat", Realm: auth.RealmCN, ContextWindow: 180_000,
			MaxOutput: 80_000, Reasoning: true, VL: false, Credits: "x1", Source: "raccoon",
			UpstreamKey: DefaultConfigName, Visible: true},
	}
}

// Catalog 返回目录缓存（进程启动即为兜底表，RefreshModels 之后换成上游真目录）。
func (c *Client) Catalog() []ModelInfo {
	c.catMu.RLock()
	defer c.catMu.RUnlock()
	if len(c.catalog) == 0 {
		return staticModels()
	}
	out := make([]ModelInfo, len(c.catalog))
	copy(out, c.catalog)
	return out
}

// ResolveModel 按 id 查目录（大小写不敏感；别名如 gpt-4o/claude-* 由 server 层先归一）。
func (c *Client) ResolveModel(model, _ string) (ModelInfo, bool) {
	model = strings.TrimSpace(model)
	for _, mi := range c.Catalog() {
		if strings.EqualFold(mi.ID, model) {
			return mi, true
		}
	}
	return ModelInfo{}, false
}

// RefreshModels 拉一次上游目录；force=false 时 10 分钟内复用缓存。
func (c *Client) RefreshModels(a *auth.Auth, force bool) ([]ModelInfo, error) {
	c.catMu.RLock()
	fresh := len(c.catalog) > 0 && time.Since(c.catAt) < 10*time.Minute
	c.catMu.RUnlock()
	if fresh && !force {
		return c.Catalog(), nil
	}
	if a == nil {
		return c.Catalog(), &Error{Kind: ErrClient, Status: 400, Msg: "没有可用账号，无法拉取模型目录"}
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, c.llmBase()+CatalogPath, nil)
	if err != nil {
		return c.Catalog(), err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.JWT())
	for key, value := range c.clientIDHeaders() {
		req.Header.Set(key, value)
	}
	resp, err := c.do(req)
	if err != nil {
		return c.Catalog(), err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return c.Catalog(), &Error{Kind: Classify(resp.Status, string(resp.Body)), Status: resp.Status,
			Msg: fmt.Sprintf("拉取模型目录失败（HTTP %d）: %s", resp.Status, truncate(errorText(resp.Body), 200))}
	}
	models, err := parseCatalog(resp.Body)
	if err != nil {
		return c.Catalog(), err
	}
	c.catMu.Lock()
	c.catalog, c.catAt = models, time.Now()
	c.catMu.Unlock()
	return models, nil
}

// parseCatalog 平铺 categories[].models；同名首次出现者胜；空 name 丢弃。
func parseCatalog(body []byte) ([]ModelInfo, error) {
	data, err := payloadOf(&apiResponse{Status: 200, Body: body}, "模型目录")
	if err != nil {
		return nil, err
	}
	rawCategories, _ := data["categories"].([]any)
	seen := map[string]bool{}
	out := make([]ModelInfo, 0, 16)
	for _, rawCategory := range rawCategories {
		category, _ := rawCategory.(map[string]any)
		models, _ := category["models"].([]any)
		for _, rawModel := range models {
			entry, _ := rawModel.(map[string]any)
			if entry == nil {
				continue
			}
			mi, ok := normalizeEntry(entry)
			if !ok || seen[mi.ID] {
				continue
			}
			seen[mi.ID] = true
			out = append(out, mi)
		}
	}
	if len(out) == 0 {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "模型目录为空"}
	}
	return out, nil
}

// normalizeEntry 把一条上游目录条目归一化。
func normalizeEntry(entry map[string]any) (ModelInfo, bool) {
	id := text(entry, "name", "model_name", "id")
	if id == "" {
		return ModelInfo{}, false
	}
	name := text(entry, "display_name", "displayDescription", "display_description", "description")
	if name == "" {
		name = id
	}
	params, _ := entry["params"].(map[string]any)
	pick := func(keys ...string) int64 {
		for _, key := range keys {
			if n, ok := int64Of(entry[key]); ok && n > 0 {
				return n
			}
			if params != nil {
				if n, ok := int64Of(params[key]); ok && n > 0 {
					return n
				}
			}
		}
		return 0
	}
	tags := []string{}
	if rawTags, ok := entry["tags"].([]any); ok {
		for _, t := range rawTags {
			if s, ok := t.(string); ok {
				tags = append(tags, s)
			}
		}
	}
	visible := true
	if v, ok := entry["visible"].(bool); ok {
		visible = v
	}
	credits := ""
	if raw, ok := entry["billing_multiplier"]; ok {
		if f, ok := floatOf(raw); ok && f > 0 {
			credits = "x" + strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.2f", f), "0"), ".")
		}
	}
	return ModelInfo{
		ID:            id,
		Name:          name,
		Realm:         auth.RealmCN,
		ContextWindow: pick("context_window", "contextWindow"),
		MaxOutput:     pick("max_tokens", "maxTokens"),
		Reasoning:     tagMatch(tags, "thinking", "reasoning"),
		Efforts:       []string{"low", "medium", "high"},
		VL:            tagMatch(tags, "vision", "image", "vl"),
		Credits:       credits,
		Source:        "raccoon",
		UpstreamKey:   id,
		Visible:       visible,
	}, true
}

func tagMatch(tags []string, wanted ...string) bool {
	for _, tag := range tags {
		for _, w := range wanted {
			if strings.EqualFold(tag, w) {
				return true
			}
		}
	}
	return false
}

func int64Of(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%g", &f); err == nil {
			return int64(f), true
		}
	}
	return 0, false
}

func floatOf(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%g", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}
