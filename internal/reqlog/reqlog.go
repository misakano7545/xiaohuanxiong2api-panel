// Package reqlog 是面板用的请求台账：一个定长环形缓冲 + 累计计数。
// 只放内存，重启即清空——它是运维观察面，不是计费账本。
package reqlog

import (
	"sync"
	"time"
)

// Event 是一次请求。
type Event struct {
	Time       string `json:"time"`
	Protocol   string `json:"protocol"` // chat / responses / messages
	Model      string `json:"model"`
	Account    string `json:"account"`
	Status     int    `json:"status"`
	Stream     bool   `json:"stream"`
	DurationMs int64  `json:"duration_ms"`
	Prompt     int64  `json:"prompt_tokens"`
	Completion int64  `json:"completion_tokens"`
	Reasoning  int64  `json:"reasoning_tokens"`
	Error      string `json:"error,omitempty"`
	TTFBMs     int64  `json:"ttfb_ms"`
}

// Recorder 是环形缓冲。
type Recorder struct {
	mu     sync.Mutex
	buf    []Event
	next   int
	full   bool
	total  int64
	errors int64
	since  time.Time
}

// New 建一个容量为 max 的台账（max <= 0 时取 500）。
func New(max int) *Recorder {
	if max <= 0 {
		max = 500
	}
	return &Recorder{buf: make([]Event, max), since: time.Now()}
}

// Add 记一条。
func (r *Recorder) Add(e Event) {
	if e.Time == "" {
		e.Time = time.Now().Format(time.RFC3339)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[r.next] = e
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
	r.total++
	if e.Status >= 400 {
		r.errors++
	}
}

// List 返回最近 limit 条（新的在前）。
func (r *Recorder) List(limit int) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.next
	if r.full {
		n = len(r.buf)
	}
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]Event, 0, limit)
	for i := 0; i < limit; i++ {
		idx := r.next - 1 - i
		if idx < 0 {
			idx += len(r.buf)
		}
		out = append(out, r.buf[idx])
	}
	return out
}

// Stats 是累计计数。
type Stats struct {
	Total    int64 `json:"total"`
	Errors   int64 `json:"errors"`
	UptimeMs int64 `json:"uptime_ms"`
}

// Stats 返回累计计数。
func (r *Recorder) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Stats{Total: r.total, Errors: r.errors, UptimeMs: time.Since(r.since).Milliseconds()}
}
