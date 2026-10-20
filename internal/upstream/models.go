package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ModelInfo 是模型目录的一条。
type ModelInfo struct {
	// ID 是给客户端用的名字：官方 display_name 去掉空格（客户端唯一需要填的值）。
	ID string `json:"id"`
	// Name 是官方 display_name（面板显示用，可能带空格）。
	Name string `json:"name"`
	// UpstreamKey 是发往上游的 model_config.key（如 qmodel_38max）。
	UpstreamKey   string   `json:"upstream_key"`
	Reasoning     bool     `json:"reasoning"`
	Efforts       []string `json:"efforts,omitempty"`
	ContextWindow int64    `json:"context_window"`
	MaxOutput     int64    `json:"max_output"`
	VL            bool     `json:"vl"`
	Enabled       bool     `json:"enabled"`
	Credits       string   `json:"credits,omitempty"`
	Realm         string   `json:"realm,omitempty"`
	Source        string   `json:"source,omitempty"`
}

// fallbackRow 是静态兜底目录的一行（官方目录拉不到时仍能出模型列表）。
type fallbackRow struct {
	id        string
	key       string
	reasoning bool
	efforts   []string
	vl        bool
	enabled   bool
	factor    string
}

var fallbackGlobal = []fallbackRow{
	{"Qwen3.8-Flash", "qfmodel", true, []string{"low", "medium", "xhigh"}, true, true, "0.1"},
	{"Qwen3.8-Max", "qmodel_38max", true, []string{"low", "medium", "xhigh"}, true, true, "0.5"},
	{"Auto", "auto", true, nil, true, false, "1"},
	{"Ultimate", "ultimate", true, nil, true, false, "1.6"},
	{"Performance", "performance", true, nil, true, false, "1.1"},
	{"Efficient", "efficient", false, nil, true, false, "0.3"},
	{"Sonus", "smodel", true, nil, true, false, "3.2"},
	{"Cantus", "cmodel", true, nil, true, false, "3.2"},
	{"Qwen3.7-Max", "qmodel_latest", true, nil, true, false, "0.5"},
	{"Qwen3.7-Plus", "qmodel", false, nil, true, false, "0.1"},
	{"Kimi-K3", "kmodel_latest", false, nil, true, false, "0.8"},
	{"Kimi-K2.8-Preview", "kmodel", false, nil, true, false, "0.3"},
	{"GLM-5.3", "gmodel", true, nil, true, false, "0.6"},
	{"GLM-5.3-Flash", "gfmodel", true, nil, true, false, "0.1"},
	{"DeepSeek-V4-Pro", "dmodel", true, nil, true, false, "0.8"},
	{"DeepSeek-Flash", "dfmodel", true, nil, true, false, "0.2"},
	{"MiniMax-M3", "mmodel", false, nil, true, false, "0.2"},
}

var fallbackCN = []fallbackRow{
	{"Qwen3.8-Flash", "qfmodel", true, []string{"low", "medium", "xhigh"}, true, true, "0.1"},
	{"Qwen3.8-Max", "qmodel_38max", true, []string{"low", "medium", "xhigh"}, true, true, "0.5"},
	{"Auto", "auto", true, nil, true, false, "1"},
	{"Qwen3.7-Max", "qmodel_latest", true, nil, true, false, "0.5"},
	{"Qwen3.7-Plus", "qmodel", true, nil, false, false, "0.1"},
	{"DeepSeek-V4-Pro", "dmodel", true, nil, false, false, "0.8"},
	{"DeepSeek-Flash", "dfmodel", false, nil, false, false, "0.2"},
	{"GLM-5.3", "gmodel", true, nil, true, false, "0.6"},
	{"GLM-5.3-Flash", "gfmodel", true, nil, true, false, "0.1"},
	{"Kimi-K3", "kmodel_latest", false, nil, true, false, "0.8"},
	{"Kimi-K2.8-Preview", "kmodel", true, nil, true, false, "0.3"},
	{"MiniMax-M3", "mmodel", false, nil, false, false, "0.2"},
}

func fallbackModels(realm string) []ModelInfo {
	rows := fallbackGlobal
	if ifCN(realm) {
		rows = fallbackCN
	}
	out := make([]ModelInfo, 0, len(rows))
	for _, row := range rows {
		out = append(out, makeModel(row.id, row.key, row.reasoning, row.efforts, row.vl, row.enabled, row.factor, realm))
	}
	return out
}

func makeModel(id, key string, reasoning bool, efforts []string, vl, enabled bool, factor, realm string) ModelInfo {
	if efforts == nil {
		efforts = []string{}
	}
	credits := ""
	if strings.TrimSpace(factor) != "" {
		credits = "x" + strings.TrimSpace(factor) + " credits"
	}
	return ModelInfo{
		ID:            id,
		Name:          id,
		UpstreamKey:   key,
		Reasoning:     reasoning,
		Efforts:       efforts,
		ContextWindow: DefaultContextWindow,
		MaxOutput:     MaxOutputTokens,
		VL:            vl,
		Enabled:       enabled,
		Credits:       credits,
		Realm:         normalRealm(realm),
		Source:        "system",
	}
}

// catalogTTL 是官方目录的缓存时长（与上游客户端同一口径：10 分钟）。
const catalogTTL = 10 * time.Minute

// RefreshModels 拉一次官方目录并写进缓存；force=false 且未过 TTL 时直接返回条数。
func (c *Client) RefreshModels(a *authState, force bool) (int, error) {
	realm := a.Realm()
	c.catMu.RLock()
	cached := c.catalog[realm]
	fetchedAt := c.catAt[realm]
	c.catMu.RUnlock()
	if !force && len(cached) > 0 && time.Since(fetchedAt) < catalogTTL {
		return len(cached), nil
	}

	url := ModelListURL(c.gatewayBase(realm))
	// GET 也必须带与签名一致的 {} body，否则上游 403。
	encoded := encodeBody([]byte("{}"))
	headers, err := buildAuthHeaders(encoded, url, a)
	if err != nil {
		return 0, err
	}
	headers["Content-Type"] = "application/json"
	headers["X-Model-Source"] = "system"

	req, err := http.NewRequest(http.MethodGet, url, strings.NewReader(string(encoded)))
	if err != nil {
		return 0, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := c.do(req)
	if err != nil {
		return 0, err
	}
	data, err := payloadOf(resp, "模型目录请求")
	if err != nil {
		return 0, err
	}
	models := parseCatalog(data, realm)
	if len(models) == 0 {
		return 0, &Error{Kind: ErrServer, Status: 502, Msg: "上游返回的模型目录为空"}
	}
	c.catMu.Lock()
	c.catalog[realm] = models
	c.catAt[realm] = time.Now()
	c.catMu.Unlock()
	return len(models), nil
}

// FetchModels 返回某账号地区的模型目录（缓存优先，缓存空时用静态兜底）。
// 实时刷新走 RefreshModels：面板「刷新模型」按钮与定时任务用它。
func (c *Client) FetchModels(a *authState) ([]ModelInfo, error) {
	realm := a.Realm()
	c.catMu.RLock()
	models := c.catalog[realm]
	c.catMu.RUnlock()
	if len(models) > 0 {
		return models, nil
	}
	if _, err := c.RefreshModels(a, false); err != nil {
		return fallbackModels(realm), err
	}
	c.catMu.RLock()
	models = c.catalog[realm]
	c.catMu.RUnlock()
	if len(models) == 0 {
		return fallbackModels(realm), nil
	}
	return models, nil
}

// Catalog 返回两地区目录的并集（面板模型页用；每区内部按 id 去重）。
func (c *Client) Catalog() []ModelInfo {
	c.catMu.RLock()
	defer c.catMu.RUnlock()
	out := []ModelInfo{}
	for _, realm := range []string{RegionGlobal, RegionCN} {
		source := c.catalog[realm]
		if len(source) == 0 {
			source = fallbackModels(realm)
		}
		seen := map[string]bool{}
		for _, model := range source {
			key := strings.ToLower(model.ID)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			model.Realm = realm
			out = append(out, model)
		}
	}
	return out
}

// ResolveModel 把客户端模型名解析成上游条目：先账号地区，再另一地区，最后静态兜底。
func (c *Client) ResolveModel(modelID, realm string) (ModelInfo, bool) {
	wanted := normalizeModelName(modelID)
	if wanted == "" {
		return ModelInfo{}, false
	}
	realm = normalRealm(realm)
	other := RegionCN
	if realm == RegionCN {
		other = RegionGlobal
	}
	c.catMu.RLock()
	catGlobal, catCN := c.catalog[RegionGlobal], c.catalog[RegionCN]
	c.catMu.RUnlock()

	for _, candidate := range []struct {
		realm  string
		models []ModelInfo
	}{
		{realm, c.catalogOf(realm, catGlobal, catCN)},
		{other, c.catalogOf(other, catGlobal, catCN)},
		{realm, fallbackModels(realm)},
		{other, fallbackModels(other)},
	} {
		if model, ok := findModel(candidate.models, wanted); ok {
			model.Realm = candidate.realm
			return model, true
		}
	}
	return ModelInfo{}, false
}

func (c *Client) catalogOf(realm string, catGlobal, catCN []ModelInfo) []ModelInfo {
	models := catGlobal
	if ifCN(realm) {
		models = catCN
	}
	if len(models) == 0 {
		return fallbackModels(realm)
	}
	return models
}

// normalizeModelName 归一模型名：小写、去掉空格/下划线/连字符（客户端常把
// "Qwen3.8-Max" 写成 "qwen3.8-max"、"Qwen 3.8 Max" 或 "qmodel_38max"）。
func normalizeModelName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		switch r {
		case ' ', '\t', '_', '-':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func findModel(models []ModelInfo, wanted string) (ModelInfo, bool) {
	for _, pick := range []func(ModelInfo) string{
		func(m ModelInfo) string { return m.ID },
		func(m ModelInfo) string { return m.Name },
		func(m ModelInfo) string { return m.UpstreamKey },
	} {
		for _, model := range models {
			if normalizeModelName(pick(model)) == wanted {
				return model, true
			}
		}
	}
	return ModelInfo{}, false
}

// parseCatalog 解析官方目录：{"chat":[{"key","display_name","is_reasoning","is_vl",
// "enable","max_input_tokens","price_factor","thinking_config":{...},"source"}...]}。
func parseCatalog(payload map[string]any, realm string) []ModelInfo {
	if message := envelopeError(payload); message != "" {
		return nil
	}
	chat, ok := payload["chat"].([]any)
	if !ok {
		return nil
	}
	out := []ModelInfo{}
	seen := map[string]bool{}
	for _, raw := range chat {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		key := strings.TrimSpace(text(item, "key"))
		display := strings.TrimSpace(text(item, "display_name"))
		if key == "" || display == "" {
			continue
		}
		id := strings.Join(strings.Fields(display), "")
		if id == "" || seen[strings.ToLower(id)] {
			continue
		}
		seen[strings.ToLower(id)] = true
		factor := ""
		if value := numberOrNil(item["price_factor"]); value != nil && *value >= 0 {
			factor = trimFloat(*value)
		}
		model := makeModel(id, key, truthy(item["is_reasoning"]) || truthy(item["thinking_config"]),
			effortsOf(item), truthy(item["is_vl"]), truthy(item["enable"]), factor, realm)
		model.Name = display
		model.Source = orDefault(text(item, "source"), "system")
		if context := int64Of(item["max_input_tokens"]); context > 0 {
			model.ContextWindow = context
		}
		out = append(out, model)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// envelopeError 取上游信封里的业务错误文案（200 或缺失视为正常）。
func envelopeError(payload map[string]any) string {
	value := numberOrNil(payload["statusCodeValue"])
	if value == nil {
		value = numberOrNil(payload["code"])
	}
	if value == nil || int64(*value) == 200 {
		return ""
	}
	message := text(payload, "message", "body")
	return fmt.Sprintf("上游返回业务错误 %d: %s", int64(*value), truncate(message, 200))
}

// effortsOf 取思考档位：thinking_config.enabled.efforts 的键集。
func effortsOf(item map[string]any) []string {
	thinking, ok := item["thinking_config"].(map[string]any)
	if !ok {
		return nil
	}
	enabled, ok := thinking["enabled"].(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := enabled["efforts"].(map[string]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for key := range raw {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func numberOrNil(value any) *float64 {
	switch typed := value.(type) {
	case float64:
		return &typed
	case int:
		f := float64(typed)
		return &f
	case int64:
		f := float64(typed)
		return &f
	case json.Number:
		if f, err := typed.Float64(); err == nil {
			return &f
		}
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(typed), "%g", &f); err == nil {
			return &f
		}
	}
	return nil
}

func int64Of(value any) int64 {
	if f := numberOrNil(value); f != nil {
		return int64(*f)
	}
	return 0
}

func trimFloat(value float64) string {
	rounded := float64(int64(value*100+0.5)) / 100
	if rounded == float64(int64(rounded)) {
		return fmt.Sprintf("%d", int64(rounded))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", rounded), "0"), ".")
}
