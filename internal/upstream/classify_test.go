package upstream

import "testing"

// 分类判据必须「宁可漏判，不可误伤」：误判的代价是给一个好号写 12 小时计划冷却、
// 或者把它记成登录态失效直到自动禁用。
func TestClassifyNoFalsePenalty(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		// 上下文太长 ≠ 额度不足（曾把 insufficient 单独当额度信号）
		{"context window insufficient", 400,
			`{"code":4001,"message":"context window is insufficient for this model"}`, ErrClient},
		{"max_tokens exceeded", 400,
			`{"error":{"message":"max_tokens exceeded: 80001 > 80000"}}`, ErrClient},
		// 真的额度不足：要能认出来
		{"积分不足", 400, `{"code":14018,"message":"积分不足"}`, ErrPlanLimit},
		{"credits exhausted", 400, `{"message":"credits exhausted"}`, ErrPlanLimit},
		{"insufficient credit", 402, `{"message":"insufficient credit balance"}`, ErrPlanLimit},
		// 403 是权限 ≠ 额度；401 是登录态
		{"403 forbidden", 403, `{"message":"insufficient permissions to access this resource"}`, ErrSessionDead},
		{"401", 401, `{"message":"unauthorized"}`, ErrSessionDead},
		// 499 是调用方断开：不能换号重发
		{"499 abort", 499, `client has aborted request`, ErrClient},
		{"408", 408, `timeout`, ErrClient},
		{"429", 429, `slow down`, ErrSoftRate},
		{"502", 502, `bad gateway`, ErrGateway},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("%s: Classify(%d) = %v，期望 %v", c.name, c.status, got, c.want)
		}
	}
}

// 流内文案分类与 HTTP 同一张表：同一报文在两条路径上「罚不罚号」必须一致。
//
// 两者允许的差异只有「不罚号的那一侧怎么叫」：HTTP 会给 4xx 一个 ErrClient（好据实回给
// 调用方），流内没有状态码、认不出来就归 ErrNone（模型/参数问题）。罚号的三种
// （计划冷却 / 软限流 / 登录态失效）则必须逐格一致——否则同一个报文在流式与非流式
// 下会给账号记两种不同的账。
func TestStreamClassifyMatchesHTTP(t *testing.T) {
	penalty := func(k ErrKind) bool {
		return k == ErrPlanLimit || k == ErrSoftRate || k == ErrSessionDead
	}
	// status 给「这条报文真会伴随的状态码」：流内没有状态码，比的是「认不认得出罚号」。
	cases := []struct {
		status int
		body   string
	}{
		{400, `{"message":"context window is insufficient for this model"}`},
		{400, `{"message":"max_tokens exceeded"}`},
		{400, `{"message":"积分不足"}`},
		{401, `{"message":"unauthorized"}`},
		{400, `{"message":"insufficient credit balance"}`},
		{429, `{"message":"rate limit exceeded"}`},
	}
	for _, c := range cases {
		a, b := Classify(c.status, c.body), classifyErrorText(c.body)
		if penalty(a) != penalty(b) || (penalty(a) && a != b) {
			t.Errorf("报文 %s（HTTP %d）: HTTP=%v 流内=%v（罚号口径应一致）", c.body, c.status, a, b)
		}
	}
	// 特别是 "token" 单独出现，不能再被判成登录态失效。
	if got := classifyErrorText(`max_tokens exceeded`); got == ErrSessionDead {
		t.Errorf("max_tokens 报文被误判成登录态失效")
	}
	if got := classifyErrorText(`context window is insufficient for this model`); got == ErrPlanLimit {
		t.Errorf("上下文过长被误判成额度不足")
	}
}
