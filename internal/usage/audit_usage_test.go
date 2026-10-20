//go:build audit

// audit_usage_test.go 只读审计的「证明测试」：断言**当前（有问题）**的行为。
// 全部纯内存 + t.TempDir()，不碰真实网络、不读线上 usage.json。
//
// 跑法：go test -tags audit -count=1 -run TestAudit -v ./internal/usage/
// 竞态口径另跑：go test -race -tags audit -count=1 -run TestAudit ./internal/usage/
// 说明：这些用例在缺陷被修好后会失败 —— 那是设计意图（断言的是「缺陷存在」）。
package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func auditFileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// TestAuditFlushFailureStopsRetryUntilNextAdd：flush 在**写盘之前**就把 dirty 清零
// （usage.go:314 在 WriteFile/Rename 之前），于是任何一次落盘失败（磁盘满 / 权限 / 目录被占）
// 都不会被后台 ticker 重试：ticker 走 flush(false)，看到 dirty=false 立刻 return
// （usage.go:307）。内存里那段用量此后只有在「下一次 Add」或「面板点刷新（Save→flush(true)）」
// 时才会重新落盘；空闲期里进程被 kill / 掉电，这段用量就没了（文件里还是上一次的旧快照），
// 而防抖落盘的设计承诺是「重启不丢」。
func TestAuditFlushFailureStopsRetryUntilNextAdd(t *testing.T) {
	dir := t.TempDir()
	// 用一个普通文件占住 usage.json 的父目录名 → MkdirAll 必失败 = 落盘失败。
	blocker := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocker, "usage.json")

	r := New(path)
	r.Add(time.Now(), "u1", "glm-5.2", delta(10, 5, 15, 30), true)
	r.flush(true) // 第一次落盘：失败（父目录建不出来）
	t.Logf("落盘失败后：目标文件存在=%v 内存里 requests=%d/%d 个桶",
		auditFileExists(path), r.Snapshot(0, nil).Totals.Requests, r.Snapshot(0, nil).Buckets)

	// 修好落盘条件，且**没有新请求**（真实场景：失败发生在那段空闲期里）。
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	r.flush(false) // 后台 ticker 的下一次尝试（30s 一次，走的正是这条）

	t.Logf("修好后后台 flush(false)：目标文件存在=%v", auditFileExists(path))
	// 失败后 dirty 不许被清：清了的话后台 ticker 就不再重试，台账只剩内存那份，
	// 空闲期掉电/被 kill 直接丢量。
	if !auditFileExists(path) {
		t.Fatal("落盘失败后后台没重试（dirty 被提前清了，台账会丢量）")
	}
	got := r.Snapshot(0, nil).Totals
	if got.Requests != 1 || got.TotalTokens != 15 {
		t.Fatalf("内存里其实还有那份用量（requests=%d tokens=%d）—— 只是没落盘", got.Requests, got.TotalTokens)
	}
	t.Logf("→ 这段用量只活在内存里：此刻 kill -9 就永久丢（flush 的 dirty 已在失败前被清掉）")

	// 唯一的恢复路径：force=true 的调用（面板「刷新」→ usageSave→Save、或 Stop 时的最后一次）。
	r.Save()
	t.Logf("面板点刷新（Save→flush(true)）后：目标文件存在=%v —— 后台那条路永远不会补写",
		auditFileExists(path))
	if !auditFileExists(path) {
		t.Fatal("force 路径本该写成功，前提变了")
	}
}

// TestAuditCorruptLedgerIsBackedUpNotOverwritten：usage.json 解析失败时 New 只在日志里写一行
// 「从零开始」（usage.go:98-100），**不向调用方返回错误** —— main 照样启动、面板照样开机；
// 紧接着的第一次落盘（30s 后或面板点刷新）就把这份坏文件**覆盖**成空台账，且不写任何备份
// （flush 只用 path+".tmp" 原子替换，usage.go:325-332）。结果是整段用量/积分估算历史不可恢复，
// 唯一线索是启动日志里那一行。
func TestAuditCorruptLedgerIsBackedUpNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	// 半截 JSON：掉电 / 坏扇区 / 被截断的写入都可能留成这样。
	corrupt := `{"version":1,"saved":"2026-10-01T00:00:00+08:00","buckets":[{"s":"h:2026-09-01T10","u":"u1","m":"glm-5.2","q":7,`
	if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	r := New(path) // 拿不到任何错误信号
	snap := r.Snapshot(0, nil)
	t.Logf("坏台账读入结果：桶=%d requests=%d（从零开始；坏文件已备份+日志）", snap.Buckets, snap.Totals.Requests)
	if snap.Buckets != 0 || snap.Totals.Requests != 0 {
		t.Fatalf("期望复现「解析失败 → 从零开始」：桶=%d", snap.Buckets)
	}

	r.Save() // 从零开始落一份合法台账（坏文件此前已备份）
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("落盘后文件应当合法: %v", err)
	}
	baks, _ := filepath.Glob(path + "*")
	t.Logf("覆盖后：文件 %d 字节 / %d 个桶；同目录同名文件: %v", len(raw), len(f.Buckets), baks)

	if len(f.Buckets) != 0 {
		t.Fatalf("坏台账应当从零开始：桶=%d", len(f.Buckets))
	}
	// 坏文件必须先被备份：直接覆盖会把「为什么坏」的唯一线索抹掉（掉电/坏扇区排障靠它）。
	corruptPath := path + ".corrupt"
	cbak, cerr := os.ReadFile(corruptPath)
	if cerr != nil {
		t.Fatalf("坏台账没有留下 .corrupt 备份: %v", cerr)
	}
	if !strings.Contains(string(cbak), `"q":7`) {
		t.Fatalf("备份里看不到原字节: %s", cbak)
	}
	t.Logf("→ 坏台账已备份为 %s（%d 字节，原坏字节还在）", filepath.Base(corruptPath), len(cbak))
}
