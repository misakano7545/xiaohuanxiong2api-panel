package upstream

import "testing"

// 限免口径：上游用 billing_status=limited_free + billing_effective_multiplier=0 标记，
// 面板要靠这两个字段画「限免」徽标并把倍率显示成 0×（原倍率划掉）。
// 普通折扣（glm-5-3：billing_multiplier 0.75 / effective 0.75、status normal）不能被当成限免。
func TestNormalizeEntryLimitedFree(t *testing.T) {
	free := map[string]any{
		"name":                         "sn-sensenova-6-8-flash",
		"display_description":          "SenseNova-6.8-Flash",
		"tags":                         []any{"general", "fast"},
		"billing_multiplier":           float64(0.5),
		"billing_effective_multiplier": float64(0),
		"billing_status":               "limited_free",
		"billing_status_note":          "限免至10月31日",
		"params":                       map[string]any{"context_window": float64(128000)},
	}
	mi, ok := normalizeEntry(free)
	if !ok {
		t.Fatal("limited_free 条目应被接受")
	}
	if mi.BillingStatus != "limited_free" || mi.FreeNote != "限免至10月31日" {
		t.Fatalf("限免字段丢失: status=%q note=%q", mi.BillingStatus, mi.FreeNote)
	}
	if mi.Credits != "x0" || mi.BaseCredits != "x0.5" {
		t.Fatalf("倍率口径错: credits=%q base=%q（期望 x0 / x0.5）", mi.Credits, mi.BaseCredits)
	}

	// 普通折扣：status normal、effective 0.75 → 原样保留，不标限免。
	discount := map[string]any{
		"name":                         "sn-glm-5-3",
		"billing_multiplier":           float64(0.75),
		"billing_effective_multiplier": float64(0.75),
		"billing_status":               "normal",
	}
	md, ok := normalizeEntry(discount)
	if !ok {
		t.Fatal("普通条目应被接受")
	}
	if md.BillingStatus != "" || md.Credits != "x0.75" || md.BaseCredits != "" {
		t.Fatalf("普通折扣被误判为限免: status=%q credits=%q base=%q", md.BillingStatus, md.Credits, md.BaseCredits)
	}

	// 没有 note 时用 billing_discounts 的名字兜底。
	noNote := map[string]any{
		"name":                         "sn-deepseek-v4-1-flash-1017",
		"billing_multiplier":           float64(0.25),
		"billing_effective_multiplier": float64(0),
		"billing_status":               "limited_free",
		"billing_discounts": []any{map[string]any{
			"name": "限免至10月31日", "multiplier": float64(0), "active": true,
		}},
	}
	mn, _ := normalizeEntry(noNote)
	if mn.FreeNote != "限免至10月31日" {
		t.Fatalf("折扣名兜底失效: note=%q", mn.FreeNote)
	}
}
