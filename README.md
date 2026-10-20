# xiaohuanxiong2api-panel

把商汤「小浣熊」（`xiaohuanxiong.com`）的 Web/桌面客户端登录态，包成一个**本地 OpenAI 兼容网关**，
同时提供 **Anthropic Messages** 与 **OpenAI Responses** 两个入口，让 Claude Code 和 Codex 这类
编码代理直接把它当模型服务用。单二进制、内嵌管理面板、多账号轮转与自动续期。

- **OpenAI SDK / curl** → `POST /v1/chat/completions`
- **OpenAI Codex / 新版 Agent** → `POST /v1/responses`
- **Claude Code / Anthropic SDK** → `POST /v1/messages`（同时挂 `/messages`）

> 本项目与商汤科技无任何关系，非官方、未获授权。请遵守上游服务条款，仅个人自用；
> 不要批量注册、转售或用于商业用途。不包含任何账号、token 或密钥。

## 为什么不是一个纯透传

小浣熊的 LLM 网关本身就是 OpenAI `chat/completions` 形状（含 SSE、`reasoning_content`、
`tool_calls`、`usage`），所以网关做的是三件事：

1. **选号与续期**：池里的账号按优先级轮转，401 先刷新同号再重试，429 冷却，5xx 换号；
2. **模型名回写**：上游会把响应帧里的 `model` 换成它内部名，网关改回客户端请求的名字；
3. **协议翻译**：`/v1/messages` 与 `/v1/responses` 各自把入站请求翻成 chat 请求、
   再把 chat 的 SSE 翻回各自的线上形状（Anthropic 的 `message_start/content_block_delta/message_stop`、
   Responses 的 `response.created/output_text.delta/completed` 等），工具调用与思维链都在两条路上对齐。

## 快速开始

```bash
git clone https://github.com/misakano7545/xiaohuanxiong2api-panel
cd xiaohuanxiong2api-panel
cp config.example.json config.json      # 按需改 listen / api_key
go build -o xhx2api ./cmd/server
./xhx2api -config ./config.json
```

```bash
# 健康检查
curl -s http://127.0.0.1:7867/healthz

# 模型列表（来自上游目录，visible:false 的不展示）
curl -s -H 'Authorization: Bearer <api_key>' http://127.0.0.1:7867/v1/models

# 一条 chat
curl -s -H 'Authorization: Bearer <api_key>' -H 'Content-Type: application/json' \
  -d '{"model":"raccoon-chat-ml-5-5","messages":[{"role":"user","content":"你好"}]}' \
  http://127.0.0.1:7867/v1/chat/completions
```

面板：浏览器打开 `http://127.0.0.1:7867/panel/`，第一次会让你填面板密钥
（`admin_key`，留空则用 `api_key`）。

### Docker

```bash
cp config.example.json config.json
docker compose up -d --build
```

`auths/` 与 `data/` 是挂载卷；容器以 uid 10001 运行，宿主机目录需要可写（`chown -R 10001`）。

## 添加账号

两种方式，都落到 `auths/<uid>.json`（0600，原子写）：

**面板「导入账号」** —— 粘贴一份 JSON：

```json
{"access_token":"eyJ…","refresh_token":"eyJ…","name":"小浣熊-1"}
```

token 从浏览器登录 `xiaohuanxiong.com` 后取：DevTools → Application → Local Storage，
或任意一次请求的 `Authorization` 头。缺 `refresh_token` 也能跑，但 access token 过期后
无法自动续期（面板会提示）。

**面板「网页登录」** —— 点开授权页登录 → 浏览器跳到
`office-raccoon://auth/callback?code=…&state=…`（页面提示打不开是正常的）→ 把整条链接粘回来。
`state` 由本服务生成、10 分钟有效、一次性消费。

## 配置

`config.json`（JSON）里的键都是**环境变量的默认值**：环境变量永远优先，文件只补空缺。

| 键 | 默认 | 说明 |
|---|---|---|
| `listen` | `127.0.0.1:7867` | 监听地址；对外暴露前先设 `api_key` |
| `api_key` | `""` | `/v1/*` 的 Bearer 密钥，空 = 不鉴权 |
| `admin_key` | `""` | 面板密钥，空则回落到 `api_key` |
| `auth_dir` | `./auths` | 账号目录 |
| `default_model` | `raccoon-chat-ml-5-5` | 请求没带 `model` 时用 |
| `models` | `[]` | 非空时作为模型白名单与 `/v1/models` 内容，不再拉上游目录 |
| `upstream.llm_base` | `https://xiaohuanxiong.com/api/web/llm/v2` | 聊天与目录 |
| `upstream.auth_base` | `…/api/web/auth/v1` | 续期 |
| `upstream.points_base` | `…/api/web/points/v1` | 积分余额 |
| `upstream.origin` | `https://xiaohuanxiong.com` | 每日积分发放（签到） |
| `upstream.request_timeout_seconds` | `0` | 单次 chat 总超时，`0` = 不设（长流式回答） |
| `pool.max_rotate` | `3` | 单请求最多试几个号 |
| `pool.cooldown_seconds` | `600` | 429/连续失败后的冷却 |
| `pool.refresh_skew_seconds` | `300` | 提前多久续期 |
| `schedule.checkin_enabled` / `checkin_hours` | `true` / `[9]` | 每日签到（`setting_info` 触发当日积分发放） |
| `schedule.balance_refresh_minutes` | `10` | 后台刷额度间隔 |

对应环境变量：`LISTEN`、`API_KEY`、`ADMIN_KEY`、`AUTH_DIR`、`DEFAULT_MODEL`、`MODELS`、
`LLM_BASE`、`AUTH_BASE`、`POINTS_BASE`、`ORIGIN`、`POOL_MAX_ROTATE`、`POOL_COOLDOWN_SECONDS`、
`POOL_REFRESH_SKEW_SECONDS`、`CHECKIN_ENABLED`、`BALANCE_REFRESH_MINUTES`。

## 接到客户端

**Claude Code**（`~/.claude/settings.json` 或环境变量）：

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:7867
export ANTHROPIC_AUTH_TOKEN=<api_key>
```

**Codex / Responses 客户端**：

```toml
# ~/.codex/config.toml
model_provider = "xhx"
model = "raccoon-chat-ml-5-5"

[model_providers.xhx]
name = "小浣熊"
base_url = "http://127.0.0.1:7867/v1"
env_key = "XHX_API_KEY"
wire_api = "responses"
```

**任何 OpenAI SDK**：`base_url=http://127.0.0.1:7867/v1`，`api_key=<api_key>`。

## 行为与边界

- **模型校验**：默认按上游目录校验（未知模型直接 400，不打上游）；目录拉不到时不拦请求，
  交给上游自己报错。`models` 非空时以白名单为准。
- **错误透传**：请求本身被上游拒（4xx 非 401/429）时不换号，照实回上游状态码与原话。
- **冷却**：429 立刻冷却；连续失败 3 次进入冷却。冷却只在运行期生效，重启清零。
- **不注入提示词**、不改消息序列（小浣熊上游不要求首条是 system）。
- **不支持 `reasoning_effort` / `thinking.budget_tokens` 的档位选择**：上游不认这类字段，
  网关不强塞；上游自己返回的 `reasoning_content` 会作为 thinking/推理内容下发。
- `/v1/chat/completions` 的请求体除 `model` 外**原样透传**（图片多模态、`tool_choice`、
  `parallel_tool_calls` 都跟着走）。
- 请求台账只放内存（最近 500 条），重启清空；它不是计费账本。

## 开发

```bash
go build ./...
go test ./...          # internal/server 用 httptest 假上游，覆盖三协议 × 流/非流 × 工具调用
go test -race ./...
```

代理结构：`internal/upstream`（上游调用）→ `internal/pool`（选号/冷却）→
`internal/server`（三协议入口）→ `internal/panel`（管理面板）。

## License

MIT，见 [LICENSE](LICENSE)。
