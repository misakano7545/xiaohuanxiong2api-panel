# Qoder2API Panel

把 [Qoder](https://qoder.com)（国际版 `qoder.com` / 国内版 `qoder.com.cn`）的编码助手通道
包装成 **OpenAI 兼容网关**，自带一个 Web 管理面板：多账号池、熔断冷却、会话粘性、
COSY 签名转发、每日签到领 Credits、额度/套餐查询、模型目录、用量台账与请求日志。

单二进制 + 内嵌页面 + 一份 JSON 配置，**纯 Go 标准库，零第三方依赖**。

> 架构与交互以 [WorkBuddy2API Panel](https://github.com/linguo2625469/workbuddy2api-panel) 为基线，
> 上游协议换成 Qoder 的 COSY 签名体系；协议事实取自 `bincooo/qoder2api`、`shuishuipingan/qoder2api-hub`、
> `nullstacks/qoder2api`、`swysgh/qoder2api` 等公开实现，并已用本仓测试固定下来。

## 功能

- **三种入站协议**：`POST /v1/chat/completions`（OpenAI chat，流式/非流式）、
  `POST /v1/responses`（Codex）、`POST /v1/messages` 与 `POST /messages`（Anthropic，支持 `x-api-key`）；
  三者都先归一成 chat 请求，再走**同一条链路**（挑号、冷却、会话粘性、提示词改写）
- **`GET /v1/models`**：模型目录（两区并集；上游 `model/list` 是 GET 也必须带 `{}` body + 签名的
  形状，本仓照实现，10 分钟缓存，拉不到回落到静态兜底）
- **双区域**：国际版走 `api3.qoder.sh`（`jt-` 令牌分流到 `api2`），国内版走 `gateway.qoder.com.cn`；
  账号地区决定模型可见性与出口
- **多账号池**：积分加权挑选（闲置补偿 + 快过期优先）、在途限额、熔断、降权、计划冷却；
  401 连续 3 次才禁用（一次抖动不杀号）
- **错误分类**（HTTP 级与流内信封共用一张表）：额度/套餐 → 计划冷却；限流 → 软冷却退避；
  登录态失效 → 会话失效计数；**排队（10605）不罚号**，换号重试即可
- **账号接入两条路**：设备授权（PKCE S256，面板给链接、浏览器授权、服务端自己轮询收号）、
  PAT / refreshToken 直登；都落到 `auths/qoder-<uid>.json`（0600，原子写回）
- **签到与额度**：`sash` 活动列表 → 领取 `CLAIM_BENEFIT`（官方幂等，已领即「今日已领取」）；
  额度按 `userQuota + addOnQuota + orgResourcePackage` 摊成「积分构成」
- **COSY 签名转发**：AES-128-CBC 身份包 + RSA(PKCS#1 v1.5) 密钥 + MD5 签名，请求体走上游
  自定义编码（`Encode=1`）；设备指纹 `cosy-machineid` 按账号稳定保存，可一键轮换
- **Token 保活 / 定时签到 / 周期余额**：排程时点可配，refresh token 轮换后原子落盘
- **可观测**：请求记录（含客户端 IP/UA，可关）、用量台账（时间片 × 账号 × 模型）、
  Prometheus 指标、运行日志环

## 快速开始

```bash
go build ./...                                          # 全量构建
CGO_ENABLED=0 go build -o q2api ./cmd/server            # 静态二进制
./q2api -config ./config.json                           # 缺配置时先写一份默认配置再起
```

- 默认监听 `:7864`，面板 `http://127.0.0.1:7864/panel/`
- 首次运行会生成 `config.json`，其中 `api_key` 是随机 `sk-…`（面板「配置」页可改）
- 账号目录不存在会自动创建；没有账号是正常启动态

Docker：

```bash
cp config.example.json config.json
docker compose up -d          # 镜像内二进制 /app/q2api，数据在 ./auths 与 ./data
```

## 用法

1. 面板 → **账号** → **添加账号**
   - 浏览器登录：选地区（国际版 / 国内版）→ 「打开」在新标签页完成 Qoder 授权 → 面板每 3 秒自动收号
   - refreshToken / PAT：另一页签直接粘（`pt-` 开头按 PAT 兑换，其余按刷新令牌续期）
2. **模型**页确认目录已拉到官方清单（拉不到会显示静态兜底并留日志）
3. 客户端指向 `http://<host>:7866/v1`，`Authorization: Bearer <api_key>`：

```bash
curl http://127.0.0.1:7866/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"Qwen3.8-Max","stream":true,
       "messages":[{"role":"user","content":"用 Go 写个快排"}]}'
```

模型名接受官方显示名、小写写法与上游 key（`Qwen3.8-Max` / `qwen3.8-max` / `qmodel_38max`）；
未知模型直接 400，不做猜测。

地区用前缀指定：`cn:Auto`（国内版）/ `global:Auto`（国际版）；不带前缀按目录归属走，
某个地区没有可用账号时会落到另一个地区。

## 端点

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/chat/completions` | OpenAI chat（流式 / 非流式） |
| POST | `/v1/responses` | Codex Responses |
| POST | `/v1/messages`、`/messages` | Anthropic Messages |
| POST | `/v1/messages/count_tokens`、`/messages/count_tokens` | 上下文 token 估算（字符/4，本地算，不占额度） |
| GET | `/v1/models` | 模型目录 |
| GET | `/status` | 账号池状态（需 key） |
| GET | `/healthz` | 存活探针（不需要 key） |
| GET | `/panel/` | 管理面板 |

面板 API 在 `/panel/api/*`（overview / models / packages / usage / request_logs / config /
login/* / accounts/* / checkin / balance / keepalive）。空 `api_key` 时只信本机回环，
写操作另外校验同源。

## 配置

见 `config.example.json`。要点：

| 键 | 说明 |
|---|---|
| `listen` / `api_key` | 监听地址；空 = 不鉴权（仅本机） |
| `auth_dir` / `state_file` | 账号目录与池状态；用量台账与请求归档默认跟 `state_file` 同目录 |
| `default_model` | 请求缺 `model` 或写 `auto` 时用，默认 `Auto` |
| `cooldown.plan_credit` | 额度不足/套餐不支持的硬冷却（默认 12h） |
| `upstream.gateway_host` / `gateway_host_cn` | 网关基址，留空用内置（`api3.qoder.sh` / `gateway.qoder.com.cn`） |
| `upstream.timeout_seconds` / `header_timeout_seconds` / `idle_timeout_seconds` | 短请求 / 首字节 / 流内空闲 |
| `pool.*` | 熔断、降权、在途、闲置补偿、快过期窗口 |
| `session_sticky.*` | 同一会话粘同一账号（命中上游缓存） |
| `prompt.mode` / `file` | `passthrough` / `custom` / `append` 系统提示词改写 |
| `schedule.*` / `logging.*` | 签到/保活时点、余额周期、请求记录与归档 |

环境变量覆盖：`Q2A_<段>_<键>`，如 `Q2A_LISTEN=0.0.0.0:7864`、`Q2A_API_KEY=sk-…`。

## 发版

版本号是上海时间 `v2026.10.19.0930`（日期不补零，时分不补零/两位）；标签、Release 与二进制
版本串同一个号。本地构建显示 `dev`，CI 用 `-X main.appVersion=<tag>` 注入；推 `v*` 标签才会
创建 Release 并推送镜像。

## 已知限制

- **国内版签到**需要设备风控身份（UMID 那一套）。本仓未移植：国内号会拿到空活动列表并
  按「今日无活动」跳过，不会伪装成功。
- **积分单价不内置**：Qoder 没有稳定可引用的积分价目，面板「积分≈」显示 `—`，不做估算
  （同名模型多，错配单价会静默给出错误金额）。
- **凭证明文落盘**（`auths/*.json`，0600）：宿主机目录本身也要收紧。
- **同一批账号不要同时挂到多个网关**（如 agent2api 与本仓）：两边各自续期 refresh token
  会互相顶掉。
- 上游接口随时可能变（COSY 版本号、信封结构、模型名）；出问题先比对 `internal/upstream/`
  的协议常量与参考实现。

## 开发

```bash
go vet ./...
go test ./...        # COSY 已知向量、凭证解析、SSE 信封、端到端假上游（流式/非流式/工具调用/签到）
```

没有真账号时联调全链路：`python3 scripts/qoder_stub.py 7899` 起假上游，另起一个实例把
`upstream.gateway_host`/`gateway_host_cn` 指向 `http://127.0.0.1:7899/`、放一份假凭证进
`auths/qoder-<uid>.json`，即可验证签名→组包→SSE 转换（桩会拒绝没有 COSY 头的请求）。
