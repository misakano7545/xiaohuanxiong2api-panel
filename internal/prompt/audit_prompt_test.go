//go:build audit

// audit_prompt_test.go 只读审计的「证明测试」：断言**当前（有问题）**的行为。
// 只做纯字符串/JSON 处理，不碰网络、不读真实提示词文件。
//
// 跑法：go test -tags audit -count=1 -run TestAudit -v ./internal/prompt/
// 说明：这些用例在缺陷被修好后会失败 —— 那是设计意图（断言的是「缺陷存在」）。
package prompt

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAuditEmptyPromptTextDisablesRewriteSilently：三处各自「合理」的代码合起来是
// 一条静默 fail-open 路径：
//
//	prompt.Load 对**可读但为空**的文件返回 ("", nil)（prompt.go:26-35：只有读失败才报错），
//	cmd/server 的 normalizePrompt 照收（不校验文本非空），
//	Rewrite/Append 见到空文本直接原样返回（prompt.go:44-46 / 85-88）。
//
// 结果：prompt.mode=custom 配一个空提示词文件（或只含空白的文件）= 网关**什么也不改写**，
// 客户端那条要抹掉的固定模板句原样发给上游 —— 而配置文件里的 mode 明明写着 custom。
// 与「文件不可读 → 启动即报错」的既定策略正好相反。
func TestAuditEmptyPromptTextDisablesRewriteSilently(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	text, err := Load("custom", empty)
	t.Logf("Load(custom, 0 字节文件) = %q, err=%v", text, err)
	// 空文件不是「合法 custom」：custom/append 会退化成静默不改写（配置文件里却写着 custom）。
	if err == nil {
		t.Fatalf("空提示词文件被当成了合法 custom（会静默不改写）: text=%q", text)
	}
	if text != "" {
		t.Fatalf("报错时不该返回文本，实得 %q", text)
	}

	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"You are TRAE SOLO, 固定模板句（本来要被抹掉的指纹）"},` +
		`{"role":"user","content":"hi"}]}`)
	// Load 已经报错，text 恒为空；空文本走 passthrough 是 Rewrite/Append 自身该有的守卫
	// （调用方拿到 err 就会 fail fast，不会走到这里）。
	out := Rewrite(body, text)
	if !bytes.Equal(out, body) {
		t.Fatalf("空文本应原样返回：%s", out)
	}
	if out2 := Append(body, text); !bytes.Equal(out2, body) {
		t.Fatalf("Append 也应当原样返回：%s", out2)
	}
	t.Logf("→ 空提示词文件在 Load 处就报错（custom/append 不会静默退化成 passthrough）")
}

// TestAuditNonArrayMessagesIsSilentlyReplaced：Rewrite 的对外契约是「解析失败 → 原样返回，
// 坏请求交给上游按原语义处理」（prompt.go:42）。但 messages 字段**存在却不是数组**时
// （对象/字符串，畸形请求的常见形态）走的是「当作没有 messages」那条分支（prompt.go:51-58）：
// 整段客户端内容被丢掉，请求被**改写成**一条只有网关 system 的合法请求 —— 畸形输入没被透传，
// 而是被静默修复了，调用方与上游都看不出中间发生过什么。
func TestAuditNonArrayMessagesIsSilentlyReplaced(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","temperature":0.2,"messages":{"role":"user","content":"保留我这段用户内容"}}`)
	out := Rewrite(body, "GW-PROMPT")
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出应当是合法 JSON: %v (%s)", err, out)
	}
	msgs, _ := got["messages"].([]any)
	t.Logf("输入 messages 是对象 → 输出 messages=%v（%d 条）", got["messages"], len(msgs))
	if len(msgs) != 2 {
		t.Fatalf("单条消息对象应归一成数组并插入网关 system（共 2 条）：%v", got["messages"])
	}
	if first, _ := msgs[0].(map[string]any); first["role"] != "system" || first["content"] != "GW-PROMPT" {
		t.Fatalf("网关 system 没插在最前：%v", msgs[0])
	}
	if !strings.Contains(string(out), "保留我这段用户内容") {
		t.Fatalf("用户内容被丢掉了：%s", out)
	}
	if got["temperature"] != 0.2 {
		t.Fatalf("其它字段没保留: %v", got["temperature"])
	}
	// messages 是 null 这类形态：不猜语义，原样返回（别把请求改成「只剩一条网关 system」）。
	nullBody := []byte(`{"model":"m","messages":null}`)
	if got := Rewrite(nullBody, "GW-PROMPT"); !bytes.Equal(got, nullBody) {
		t.Fatalf("messages=null 应原样返回，实得：%s", got)
	}
}
