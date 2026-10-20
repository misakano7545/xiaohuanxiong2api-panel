package upstream

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/misakano7545/qoder2api-panel/internal/cosy"
)

// 本文件收录「抓包实证」的网关探测与排队查询。
//
// 结论来自 Qoder 0.4.8 客户端的实流量（MITM 抓包 + 逆向）：
//   - 客户端启动时对 center/api2/api3/openapi/repo2/qts2 逐个打
//     /algo/api/v1/ping，取最快回应当出口；
//   - 端点表只在 center.qoder.sh 的 /algo/api/v5/service/region/endpoints；
//   - **排队态不是靠错误码判断的**：上游接受请求后客户端轮询
//     /algo/api/v2/service/ask/queue/status，用 isQueued / waitTime 决定是否继续等。
//     旧实现只在错误码里找 10605，队列态因此会退化成「发出去没响应」。

// QueueState 是一次排队查询的结果。
type QueueState struct {
	Queued     bool   `json:"isQueued"`
	ModelKey   string `json:"modelKey"`
	QueueType  string `json:"queueType"`
	Count      int64  `json:"queueCount"`
	WaitTime   int64  `json:"waitTime"`
	RetryAfter int64  `json:"retryAfterSeconds"`
	Available  bool   `json:"serviceAvailable"`
}

// QueueTypeOf 把模型档位映射成上游的 queueType。
// 实测 qfmodel / dfmodel 走 p3；未知档位按 p3 处理（上游接受该值）。
func QueueTypeOf(modelKey string) string {
	switch modelKey {
	case "ultimate", "quest-ultimate", "experts-ultimate":
		return "p0"
	case "performance", "qmodel_38max", "qmodel_latest", "dmodel", "gmodel":
		return "p1"
	case "smodel", "cmodel", "kmodel", "kmodel_latest", "gfmodel", "mmodel":
		return "p2"
	}
	return "p3"
}

// QueueStatus 查询某次请求的排队位置。requestSetID 取自请求体的
// request_set_id（本仓与 chat_record_id 同值）。
//
// 返回的 RetryAfter 是上游建议的轮询间隔（秒），调用方按它退避。
func (c *Client) QueueStatus(a *authState, requestSetID, modelKey string) (QueueState, error) {
	var state QueueState
	if requestSetID == "" || modelKey == "" {
		return state, &Error{Kind: ErrClient, Status: 400, Msg: "排队查询缺少 requestSetId 或 modelKey"}
	}
	query := url.Values{}
	query.Set("requestSetId", requestSetID)
	query.Set("modelKey", modelKey)
	query.Set("queueType", QueueTypeOf(modelKey))
	full := joinURL(c.inferenceBase(a.Realm(), a.JWT()), QueueStatusPath) + "?" + query.Encode()

	headers, err := buildAuthHeaders(nil, full, a)
	if err != nil {
		return state, &Error{Kind: ErrServer, Status: 500, Msg: err.Error()}
	}
	req, err := http.NewRequest(http.MethodGet, full, nil)
	if err != nil {
		return state, &Error{Kind: ErrServer, Status: 500, Msg: "构造排队查询失败"}
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")

	raw, err := c.do(req)
	if err != nil {
		return state, err
	}
	data, err := payloadOf(raw, "排队查询")
	if err != nil {
		return state, err
	}
	state.Queued = truthy(data["isQueued"])
	state.Count = intOr(data, "queueCount")
	state.WaitTime = intOr(data, "waitTime")
	state.RetryAfter = intOr(data, "retryAfterSeconds")
	state.Available = truthy(data["serviceAvailable"])
	state.ModelKey = text(data, "modelKey")
	state.QueueType = text(data, "queueType")
	return state, nil
}

// WaitQueue 轮询队列直到出队或超出预算，返回最后一次状态。
// 上游建议 2 秒一次；预算耗尽就返回排队态（调用方按「换号重试」处理，不罚号）。
func (c *Client) WaitQueue(a *authState, requestSetID, modelKey string, budget time.Duration) (QueueState, error) {
	deadline := time.Now().Add(budget)
	for {
		state, err := c.QueueStatus(a, requestSetID, modelKey)
		if err != nil {
			return state, err
		}
		if !state.Queued || time.Now().After(deadline) {
			return state, nil
		}
		wait := time.Duration(state.RetryAfter) * time.Second
		if wait <= 0 {
			wait = 2 * time.Second
		}
		time.Sleep(wait)
	}
}

// ExchangeJobToken 用设备令牌（dt-）换取作业令牌（jt-）。
//
// 实测（国内版登录链路）：
//   POST /api/v1/me/jobToken
//   Authorization: Bearer dt-…
//   {"clientId":"732aef47-9cf2-46a2-95fe-4cebb5d0d1fa"}
//
// 返回体里的令牌字段兼容几种常见命名（device_token / job_token / token）。
func (c *Client) ExchangeJobToken(a *authState, clientID string) (string, error) {
	if clientID == "" {
		clientID = QoderClientID
	}
	body, err := json.Marshal(map[string]string{"clientId": clientID})
	if err != nil {
		return "", &Error{Kind: ErrServer, Status: 500, Msg: "构造 jobToken 请求失败"}
	}
	req, err := http.NewRequest(http.MethodPost, c.apiBase(a.Realm())+JobTokenPath, bytes.NewReader(body))
	if err != nil {
		return "", &Error{Kind: ErrServer, Status: 500, Msg: "构造 jobToken 请求失败"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.JWT())
	for key, value := range OpenAPIHeaders(a.JWT()) {
		req.Header.Set(key, value)
	}
	raw, err := c.do(req)
	if err != nil {
		return "", err
	}
	data, err := payloadOf(raw, "作业令牌兑换")
	if err != nil {
		return "", err
	}
	// 有些部署把结果包在 data/payload 里。
	for _, key := range []string{"data", "payload", "result"} {
		if nested, ok := data[key].(map[string]any); ok {
			for k, v := range nested {
				if _, exists := data[k]; !exists {
					data[k] = v
				}
			}
		}
	}
	token := text(data, "device_token", "deviceToken", "job_token", "jobToken", "token", "access_token")
	if token == "" {
		return "", &Error{Kind: ErrServer, Status: 502, Msg: "作业令牌兑换响应缺少令牌"}
	}
	return token, nil
}

// RegionEndpoints 是 /algo/api/v5/service/region/endpoints 的响应形状。
type RegionEndpoints struct {
	CenterNodes  []EndpointNode `json:"centerNodes"`
	InferNodes   []EndpointNode `json:"inferNodes"`
	OpenAPINodes []EndpointNode `json:"openapiNodes"`
	Security     []EndpointNode `json:"security"`
	Codebase     []EndpointNode `json:"codebase"`
	RemoteAgent  []EndpointNode `json:"remoteAgent"`
	NesNodes     []EndpointNode `json:"nesNodes"`
	FastMode     []EndpointNode `json:"fastMode"`
}

// EndpointNode 是端点表里的一项。
type EndpointNode struct {
	URL  string `json:"url"`
	Type string `json:"type"`
}

// FetchRegionEndpoints 拉分区端点表。响应体是 COSY 自定义字母表编码（即使没标
// Encode 也照样编码），必须先 DecodeBody 再解 JSON —— 与推理接口正好相反。
func (c *Client) FetchRegionEndpoints(a *authState) (RegionEndpoints, error) {
	var out RegionEndpoints
	full := joinURL(CenterGlobal, RegionEndpointsPath)
	req, err := http.NewRequest(http.MethodGet, full, nil)
	if err != nil {
		return out, &Error{Kind: ErrServer, Status: 500, Msg: "构造端点表请求失败"}
	}
	// 实测这族请求只带身份头，不参与 COSY 签名。
	req.Header.Set("Authorization", "Bearer "+a.JWT())
	req.Header.Set("Cosy-Version", OpenAPICosyVersion)
	req.Header.Set("Cosy-ClientType", SashClientType)
	req.Header.Set("Cosy-MachineId", a.MachineIDValue())
	req.Header.Set("Cosy-MachineToken", a.MachineIDValue())
	req.Header.Set("Cosy-MachineOS", cosy.MachineOS())
	req.Header.Set("x-gw-user-id", a.UID)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "identity")

	raw, err := c.do(req)
	if err != nil {
		return out, err
	}
	if raw.Status < 200 || raw.Status >= 300 {
		return out, &Error{Kind: Classify(raw.Status, string(raw.Body)), Status: raw.Status, Msg: "端点表查询失败"}
	}
	decoded, err := cosy.DecodeBody(raw.Body)
	if err != nil {
		return out, &Error{Kind: ErrServer, Status: 502, Msg: "端点表解码失败"}
	}
	if err := json.Unmarshal(decoded, &out); err != nil {
		return out, &Error{Kind: ErrServer, Status: 502, Msg: "端点表不是合法 JSON"}
	}
	return out, nil
}

// Ping 探测一个网关是否在线（/algo/api/v1/ping，不需要签名和身份）。
// 返回往返耗时；出错时 err 非空。
func (c *Client) Ping(base string) (time.Duration, error) {
	start := time.Now()
	req, err := http.NewRequest(http.MethodGet, joinURL(base, PingPath), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "*/*")
	raw, err := c.do(req)
	if err != nil {
		return 0, err
	}
	if raw.Status < 200 || raw.Status >= 300 {
		return 0, &Error{Kind: Classify(raw.Status, string(raw.Body)), Status: raw.Status, Msg: "网关探活失败"}
	}
	return time.Since(start), nil
}

// intOr 从 map 里取一个整数（容忍数字与字符串两种形态）。
func intOr(data map[string]any, key string) int64 {
	if value, ok := asInt64(data[key]); ok {
		return value
	}
	return 0
}
