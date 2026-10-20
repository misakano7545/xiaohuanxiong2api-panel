// model_alias.go 客户端自带的模型名 → 本项目真能调的模型。
//
// 做法照社区现成实现（ZedeX/trae-local-api 的 model-config.json：客户端名字 → config_name 表；
// linqiu919/trae2api 的 convertModelName 同理）：Claude Code / Cursor / Cline 这类工具把
// claude-*、gpt-4o 写死在配置里，别名层让它们零改造接进来，不用手改每个客户端的模型名。
//
// ponytail: 只登记「目标确实存在且实测出正文」的映射；这里不做智能路由、不做 tier 回退
// （参考实现有 tiers/fallback 那套，等真需要多家竞速再加）。
package server

import (
	"strings"

	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/auth"
	"github.com/misakano7545/xiaohuanxiong2api-panel/internal/upstream"
)

// strongModel 各地区的旗舰模型：别名没有更贴切的目标时就落到它。
func strongModel(realm string) string {
	if realm == auth.RealmIntl {
		return upstream.DefaultConfigNameIntl
	}
	return upstream.DefaultConfigName
}

// modelAliases 显式别名（小写、已去 __ 后缀）。
// 小浣熊只有一套模型 id；这里把客户端写死的名字落到目录里的第一候选，
// 目标不存在时 handler 会照实 400，而不是编一个上游不认的名字。
var modelAliases = map[string]map[string]string{
	auth.RealmCN: {
		"gpt-4o": "raccoon-chat-ml-5-5", "gpt-4o-mini": "raccoon-chat-ml-5-5",
		"gpt-4.1": "raccoon-chat-ml-5-5", "gpt-4": "raccoon-chat-ml-5-5",
		"gpt-3.5-turbo": "raccoon-chat-ml-5-5", "gpt-5": "raccoon-chat-ml-5-5",
	},
}

// otherRealm 另一个地区（别名兜底用）。
// aliasModel 查别名。claude-* 走前缀（Claude 客户端会带日期后缀，逐个登记不划算）。
// 入参可能是 normalizeModelName 出来的「Claude-sonnet-4-5」（首字母被大写），统一转小写再比。
// 本地区表里没有就查另一地区：网页端显示名（Gemini-3.1-Pro-Preview）不带地区信息，
// 命中的目标名会由调用方回查 modelRealm 决定真正发哪个地区的号。
func aliasModel(realm, model string) (string, bool) {
	model = strings.ToLower(model)
	if strings.HasPrefix(model, "claude") {
		return strongModel(realm), true
	}
	if m, ok := modelAliases[realm][model]; ok {
		return m, true
	}
	return "", false
}
