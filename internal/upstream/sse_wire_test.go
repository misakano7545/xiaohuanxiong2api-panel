package upstream

import (
	"os"
	"strings"
	"testing"
)

// 本文件用**真实抓包的 SSE 响应流**验证信封解析。
// 样本来源：Qoder CN 0.4.8 客户端 chat（api6.qoder.com.cn），
//   - sse_chat_ok.stream       正常一轮：N 个 chunk + finish(stop) + [DONE] + event:finish
//   - sse_chat_quota_429.stream 额度耗尽：信封 statusCodeValue=429，body 里 details 是
//     JSON 字符串，内层 error.type=insufficient_quota，最后还有 event:error 栈

func readFrames(t *testing.T, name string) []string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("读样本失败: %v", err)
	}
	// 按 SSE 事件块切分，取每块的 data: 行（与生产 reader 的切法一致）。
	var frames []string
	for _, block := range strings.Split(string(raw), "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimRight(line, "\r")
			if strings.HasPrefix(line, "data:") {
				frames = append(frames, strings.TrimPrefix(line, "data:"))
			}
		}
	}
	if len(frames) == 0 {
		t.Fatalf("%s 没解出任何 data 帧", name)
	}
	return frames
}

// TestRealStreamChatNormal 正常流：chunk 能被解析，[DONE] 能被识别，
// 且 event:finish 那种「没有 body 的信封」必须被安全跳过而不是报错。
func TestRealStreamChatNormal(t *testing.T) {
	frames := readFrames(t, "sse_chat_ok.stream")

	var chunks, done, skipped, errors int
	for _, frame := range frames {
		ev := parseSSELine(frame)
		switch ev.Kind {
		case sseChunk:
			chunks++
			if ev.Chunk == nil {
				t.Fatalf("sseChunk 的 Chunk 为 nil")
			}
		case sseDone:
			done++
		case sseError:
			errors++
		case sseSkip:
			skipped++
		}
	}
	if errors != 0 {
		t.Errorf("正常流里不该出现业务错误，得到 %d 条", errors)
	}
	if chunks == 0 {
		t.Errorf("一个 chunk 都没解出来")
	}
	if done != 1 {
		t.Errorf("[DONE] 应恰好 1 次，得到 %d", done)
	}
	// event:finish 的 {"firstTokenDuration":...} 没有 body，必须走 skip。
	if skipped == 0 {
		t.Errorf("event:finish 之类的空信封应被跳过")
	}

	// finish_reason 必须是 stop（客户端拿这个收尾）。
	var sawStop bool
	for _, frame := range frames {
		ev := parseSSELine(frame)
		if ev.Kind != sseChunk || ev.Chunk == nil {
			continue
		}
		choices, _ := ev.Chunk["choices"].([]any)
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			if choice["finish_reason"] == "stop" {
				sawStop = true
			}
		}
	}
	if !sawStop {
		t.Errorf("没看到 finish_reason=stop")
	}
}

// TestRealStreamQuotaExceeded 额度耗尽：信封必须被识别成「计划/额度」类错误。
// 这条样本里 statusCodeValue=429，但真正的语义在 body.details 的
// insufficient_quota 里 —— 只看 HTTP 429 会误判成「限流、退避就好」。
func TestRealStreamQuotaExceeded(t *testing.T) {
	frames := readFrames(t, "sse_chat_quota_429.stream")

	var found *sseEvent
	for _, frame := range frames {
		ev := parseSSELine(frame)
		if ev.Kind == sseError {
			e := ev
			found = &e
			break
		}
	}
	if found == nil {
		t.Fatalf("没识别出错误信封")
	}
	if found.Status != 429 {
		t.Errorf("statusCodeValue = %d, want 429", found.Status)
	}
	if found.Class != ErrPlanLimit {
		t.Errorf("insufficient_quota 应归为 ErrPlanLimit（额度/套餐），得到 %v", found.Class)
	}
	if !strings.Contains(found.Raw, "insufficient_quota") {
		t.Errorf("Raw 应保留内层错误文本，得到 %q", found.Raw)
	}

	// event:error 那段栈不能被当成 chunk 或错误重复上报。
	for _, frame := range frames {
		if !strings.Contains(frame, "stackTrace") {
			continue
		}
		if ev := parseSSELine(frame); ev.Kind == sseChunk {
			t.Errorf("stackTrace 帧不该被当成 chunk")
		}
	}
}
