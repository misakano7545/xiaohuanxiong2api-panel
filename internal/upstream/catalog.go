package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Model 是归一化后的目录条目。
type Model struct {
	ID                string   `json:"id"`
	DisplayName       string   `json:"display_name"`
	Description       string   `json:"description,omitempty"`
	Tags              []string `json:"tags,omitempty"`
	MaxInputTokens    *int64   `json:"max_input_tokens,omitempty"`
	MaxOutputTokens   *int64   `json:"max_output_tokens,omitempty"`
	BillingMultiplier *float64 `json:"billing_multiplier,omitempty"`
	Visible           bool     `json:"visible"`
}

// Catalog 拉取上游模型目录（GET {llm_base}/model_catalog）。
func (c *Client) Catalog(ctx context.Context, token string) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	status, data, err := c.doJSON(ctx, http.MethodGet, c.opts.LLMBase+"/model_catalog", nil, token,
		map[string]string{"Accept": "application/json"}, maxJSONBytes)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, newError(status, data)
	}
	return parseCatalog(data)
}

type catalogEntry struct {
	Name                       string          `json:"name"`
	ModelName                  string          `json:"model_name"`
	DisplayName                string          `json:"display_name"`
	Description                string          `json:"description"`
	DisplayDescription         string          `json:"display_description"`
	Tags                       []string        `json:"tags"`
	Visible                    *bool           `json:"visible"`
	ContextWindow              json.RawMessage `json:"context_window"`
	MaxTokens                  json.RawMessage `json:"max_tokens"`
	BillingMultiplier          *float64        `json:"billing_multiplier"`
	BillingEffectiveMultiplier *float64        `json:"billing_effective_multiplier"`
	Params                     *struct {
		ContextWindow json.RawMessage `json:"context_window"`
		MaxTokens     json.RawMessage `json:"max_tokens"`
	} `json:"params"`
}

// parseCatalog 平铺 categories[].models；同名首次出现者胜；空 name 丢弃。
func parseCatalog(data []byte) ([]Model, error) {
	var probe struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("解析模型目录: %w", err)
	}
	if probe.Code != nil && *probe.Code != 0 {
		return nil, fmt.Errorf("模型目录返回异常 code=%d", *probe.Code)
	}
	root := data
	if len(probe.Data) > 0 && string(probe.Data) != "null" {
		root = probe.Data
	}
	var parsed struct {
		Categories []struct {
			Models []catalogEntry `json:"models"`
		} `json:"categories"`
	}
	if err := json.Unmarshal(root, &parsed); err != nil {
		return nil, fmt.Errorf("解析模型目录: %w", err)
	}
	out := make([]Model, 0, 16)
	seen := map[string]bool{}
	for _, cat := range parsed.Categories {
		for i := range cat.Models {
			m, ok := normalizeEntry(cat.Models[i])
			if !ok || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			out = append(out, m)
		}
	}
	return out, nil
}

func normalizeEntry(e catalogEntry) (Model, bool) {
	id := strings.TrimSpace(e.Name)
	if id == "" {
		id = strings.TrimSpace(e.ModelName)
	}
	if id == "" {
		return Model{}, false
	}
	name := strings.TrimSpace(e.DisplayName)
	if name == "" {
		name = strings.TrimSpace(e.DisplayDescription)
	}
	if name == "" {
		name = strings.TrimSpace(e.Description)
	}
	if name == "" {
		name = id
	}
	if r := []rune(name); len(r) > 64 {
		name = string(r[:64])
	}
	pick := func(top, nested json.RawMessage) *int64 {
		if n, ok := flexInt(top); ok {
			return &n
		}
		if n, ok := flexInt(nested); ok {
			return &n
		}
		return nil
	}
	var nestedCtx, nestedMax json.RawMessage
	if e.Params != nil {
		nestedCtx, nestedMax = e.Params.ContextWindow, e.Params.MaxTokens
	}
	tags := e.Tags
	if tags == nil {
		tags = []string{}
	}
	visible := true
	if e.Visible != nil {
		visible = *e.Visible
	}
	return Model{
		ID:                id,
		DisplayName:       name,
		Description:       strings.TrimSpace(e.DisplayDescription),
		Tags:              tags,
		MaxInputTokens:    pick(e.ContextWindow, nestedCtx),
		MaxOutputTokens:   pick(e.MaxTokens, nestedMax),
		BillingMultiplier: e.BillingEffectiveMultiplier,
		Visible:           visible,
	}, true
}

// flexInt 取整数字段：上游目录里数字与数字字符串混着出现。
func flexInt(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var num json.Number
	if json.Unmarshal(raw, &num) == nil {
		if v, err := num.Float64(); err == nil {
			return int64(v), true
		}
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil {
			return int64(v), true
		}
	}
	return 0, false
}
