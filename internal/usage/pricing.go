// pricing.go 单请求积分推算（**估算**，不是上游回报值）。
//
// 上游 token_usage 帧里只有 token 数，没有积分字段；面板那列一律带「≈」。
//
// Qoder 的积分单价没有可引用的公开表（峰谷倍率按账号/活动变），所以本仓**不内置单价表**：
// EstimateCredit 一律返回 ok=false，面板显示「—」——宁可空着，也不要拿别家模型的价格
// 编一个看起来像真的数字（Qoder 与别家有大量同名模型，错配会静默给出错误金额）。
// 拿到可信单价后填进 listPricesCNY 即可，公式不用改。
package usage

import "strings"

// listPricesCNY 官方刊例价（元 / 百万 token）：(输入, 输出, 缓存命中)。
// 空表 = 不推算（见包注释）。key 用模型 id 小写。
var listPricesCNY = map[string][3]float64{}

// measuredDiscount / measuredOverride 是为实测拟合留的两个修正档，空表 = 不修正。
var measuredDiscount = map[string]float64{}
var measuredOverride = map[string][3]float64{}
// modelKey 把网关的模型标识归一成价目表的键：去空白、转小写，并剥掉 CN 域前缀。
//
// 为什么要剥 "cn:"：同一条请求在网关里有两种入账形态——resolveModel 之后是裸名
// （glm-5.2），而按入站原样记录时带域前缀（cn:glm-5.2）。不归一就会出现「同一个模型
// 一半请求有积分估算、一半没有」的错觉。**intl: 不剥**：国际版与 CN 的计费口径不同，
// 价目表里只有人民币价，拿 CN 价去估国际版等于编数（宁可不估）。
func modelKey(model string) string {
	key := strings.ToLower(strings.TrimSpace(model))
	key = strings.TrimPrefix(key, "cn:")
	return key
}

// EffectivePrices 模型 → 积分单价 (输入, 输出, 缓存命中) / 百万 token；未收录返回 false。
// 优先级：实测覆盖 > 刊例 × 40 × 折扣 > 刊例 × 40。
func EffectivePrices(model string) ([3]float64, bool) {
	key := modelKey(model)
	if v, ok := measuredOverride[key]; ok {
		return v, true
	}
	entry, ok := listPricesCNY[key]
	if !ok {
		return [3]float64{}, false
	}
	f := discountOf(key)
	return [3]float64{entry[0] * f, entry[1] * f, entry[2] * f}, true
}

func discountOf(key string) float64 {
	if d, ok := measuredDiscount[key]; ok {
		return d
	}
	return 1.0
}

// EstimateCredit 按官方公式推算一组 token 的积分（≈，4 位小数）。
// 模型未收录或没给输入 token 时返回 false —— 宁可不显示，也不拿错单价误导。
// cached 会被夹进 [0, prompt]：上游没报时传 0。
func EstimateCredit(model string, prompt, completion, cached int64) (float64, bool) {
	prices, ok := EffectivePrices(model)
	if !ok || prompt < 0 {
		return 0, false
	}
	if cached < 0 {
		cached = 0
	}
	if cached > prompt {
		cached = prompt
	}
	if completion < 0 {
		completion = 0
	}
	total := float64(prompt-cached)*prices[0] + float64(completion)*prices[1] + float64(cached)*prices[2]
	return round4(total / 1e6), true
}

// round4 保留 4 位小数：计费量子为 0.0004，4 位足够，也避免浮点长尾。
func round4(v float64) float64 {
	return float64(int64(v*10000+0.5)) / 10000
}
