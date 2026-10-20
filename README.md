# xiaohuanxiong2api-panel

把商汤「小浣熊」（`xiaohuanxiong.com`）的 Web/桌面客户端登录态，包成**本地 OpenAI 兼容网关**，
同时提供 **Anthropic Messages** 与 **OpenAI Responses** 两个入口，让 Claude Code / Codex 这类
编码代理直接把它当模型服务用。单二进制、内嵌管理面板、多账号轮转与自动续期。

本仓是 [workbuddy2api-panel](https://github.com/misakano7545/workbuddy2api-panel) 的**严格派生**：
目录结构、面板（`internal/panel/index.html` + `app.js`）、账号池、调度器、三协议入口全部沿用基线，
只把「上游适配层」换成小浣熊：`internal/upstream`（HTTP/SSE/目录/额度/签到）与
`internal/auth`（access/refresh JWT 凭证）。

- **OpenAI SDK / curl** → `POST /v1/chat/completions`
- **OpenAI Codex / 新版 Agent** → `POST /v1/responses`
- **Claude Code / Anthropic SDK** → `POST /v1/messages`（同时挂 `/messages`、`/v1/messages/count_tokens`）

> 本项目与商汤科技无任何关系，非官方、未获授权。请遵守上游服务条款，仅个人自用。
> 仓库不含任何账号、token 或密钥。

## 小浣熊上游是什么形状

| 能力 | 上游 | 说明 |
|---|---|---|
| 对话 | `POST {llm_base}/chat/completions` | 就是 OpenAI 形状；SSE 是标准 `data:` 帧 |
| 续期 | `POST {auth_base}/refresh` | body `{"refresh_token":…}`，回 `data.access_token/refresh_token` |
| 目录 | `GET {llm_base}/model_catalog` | `data.categories[].models[]`，`visible:false` 不展示 |
| 积分 | `GET {points_base}/balance` | `data.available_points` + 四个钱包字段 |
| 签到 | `GET {origin}/api/web/office/v3/setting_info` | 触发当日积分发放，幂等 |
| 换码登录 | `POST {auth_base}/login_with_authorization_code` | 用 `office-raccoon://` 回调里的 code 换票 |

网关只做四件事：选号与续期、把响应帧里的 `model` 改回客户端请求的名字、
把「请求本身被拒」与「账号故障」分开、把 chat 的 SSE 翻译成 Anthropic / Responses 的线上形状。
出站聊天请求体除 `model` 外**原样透传**（多模态、`tool_choice`、`parallel_tool_calls` 都跟着走）。

## 快速开始

```bash
git clone https://github.com/misakano7545/xiaohuanxiong2api-panel
cd xiaohuanxiong2api-panel
cp config.example.json config.json     # 按需改 listen / api_key
go build -o xhx2api ./cmd/server
./xhx2api -config ./config.json
```

```bash
curl -s http://127.0.0.1:7867/healthz
curl -s -H 'Authorization: Bearer <api_key>' http://127.0.0.1:7867/v1/models
curl -s -H 'Authorization: Bearer <api_key>' -H 'Content-Type: application/json' \
  -d '{"model":"raccoon-chat-ml-5-5","messages":[{"role":"user","content":"你好"}]}' \
  http://127.0.0.1:7867/v1/chat/completions
```

面板：`http://127.0.0.1:7867/panel/`，输入 `api_key` 进入（与 `/v1` 同一把钥匙）。

Docker：`docker compose up -d --build`（`auths/`、`data/`、`config.json` 都挂出来）。

## 加账号

**导入凭证**（面板「导入凭证」）接受三种粘贴：

1. 账号 JSON：`{"access_token":"eyJ…","refresh_token":"eyJ…","nickname":"小浣熊-1"}`
2. 只有 access_token：直接粘 `eyJ…`（能用，但过期后无法自动续期）
3. 授权回调链接：`office-raccoon://auth/callback?code=…&state=…`

第 3 种来自浏览器：登录 `xiaohuanxiong.com` → 打开 `{origin}/code/authorize?login_source=desktop` →
浏览器跳到 `office-raccoon://auth/callback?...`（提示打不开是正常的）→ 复制整条链接。

小浣熊**没有**设备码授权，所以面板「添加账号」的设备流入口会如实报错并指向「导入凭证」；
这是上游协议差异，不是缺功能。账号文件落在 `auths/<uid>.json`（0600，原子写），
`refresh_token` 每次续期都轮换，落盘失败会在日志里点名（新票只在内存里，重启后需重新登录）。

## 配置

`config.json` 的每个键都能用 `Q2A_<大写>` 环境变量覆盖（环境变量优先）。

| 键 | 默认 | 说明 |
|---|---|---|
| `listen` | `127.0.0.1:7867` | 监听地址 |
| `api_key` | `""` | `/v1/*` 与面板的密钥；空 = 不鉴权（仅本机） |
| `auth_dir` / `state_file` | `./auths` / `./data/state.json` | 账号目录与池状态 |
| `default_model` | `raccoon-chat-ml-5-5` | 请求没带 model 时用 |
| `upstream.llm_base` / `auth_base` | 内置常量 | 推理网关 / 鉴权基址 |
| `upstream.points_base` / `origin` | 内置常量 | 积分 / 主站（签到） |
| `upstream.timeout_seconds` / `header_timeout_seconds` / `idle_timeout_seconds` | `120/120/300` | 短请求总超时 / 首字节超时 / 流内空闲 |
| `cooldown.plan_credit` / `soft_rate` / `soft_rate_max` | `12h/60s/2h` | 额度、限流冷却与退避封顶 |
| `pool.*` | 见示例 | 在途上限、熔断/降权阈值、闲置补偿 |
| `schedule.checkin_hours` / `keepalive_hours` | `[9]` / `[3]` | 签到与保活时点 |

## 接到客户端

**Claude Code**

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:7867
export ANTHROPIC_AUTH_TOKEN=<api_key>
```

**Codex**

```toml
model_provider = "xhx"
model = "raccoon-chat-ml-5-5"

[model_providers.xhx]
name = "小浣熊"
base_url = "http://127.0.0.1:7867/v1"
env_key = "XHX_API_KEY"
wire_api = "responses"
```

**任何 OpenAI SDK**：`base_url=http://127.0.0.1:7867/v1`。

## 行为与边界

- **模型名**：`/v1/models` 来自上游目录（`visible:false` 不展示），目录拉不到时用内置兜底表；
  未知模型直接 400，不打上游。客户端写死的 `gpt-4o` 一类名字映射到 `raccoon-chat-ml-5-5`。
- **错误口径**：401 先续期同号再重试一次，429 软冷却，5xx 熔断换号；请求本身被上游拒（其余 4xx）
  不换号，照实回上游状态码与原话。流里 `{"error":…}` 不算成功（会触发换号）。
- **非流式**：上游对 `stream:false` 回普通 JSON，网关把它摊成同形状的一帧再走同一套管道。
- **不注入提示词**、不改消息序列。
- 上游不认 `reasoning_effort` 档位；它自己回的 `reasoning_content` 会作为 thinking/推理内容下发。
- 积分单价没有可信公开表，用量页只记 token，不编造金额。

## 开发

```bash
go build ./...
go test ./...          # internal/server 用 httptest 假上游覆盖三协议 × 流/非流 × 工具调用
go test -race ./...
```

## License

MIT，见 [LICENSE](LICENSE)。基线来自 workbuddy2api-panel（同一作者）。
