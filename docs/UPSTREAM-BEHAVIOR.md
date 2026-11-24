# 上游错误分类与处置

这份文档是 `internal/relay/errors.go` 与 `internal/pool/cooldown.go` 的行为说明，
也是排查「为什么这个号被冷却 / 为什么这个请求没换号」时的第一入口。

## 分类顺序

业务码优先于 HTTP 状态码。上游常把业务错误塞进 HTTP 200 的流里（`Status` 为 0），
只看状态码会把明确的限流/拒答错报成网络问题。

| 顺序 | 判据 | 分类 | 换号 | 冷却 | 对外 |
|---|---|---|---|---|---|
| 1 | 客户端断开 / 写回失败 | other | 否 | 否 | 499 |
| 2 | 排队超时 | other | 否 | 否 | 503 |
| 3 | 上下文超长（11115 / 文案） | other | 否 | 否 | 400 |
| 4 | 请求体过大（413 / 文案） | other | 否 | 否 | 413 |
| 5 | Schema 非法（4027） | other | 否 | 否 | 400 |
| 6 | 敏感词拒答（100041 / 100042 / 内容策略文案） | rejected | 否 | 否 | 400 |
| 7 | 积分不足（1000007 / 名字码 / 429+余额文案） | quota | 是 | 整号至下次签到 | 402 |
| 8 | 账号异常（200108） | account_abnormal | 是 | 账号级阶梯 | 429 |
| 9 | 401 / Auth 标记 / 200003 / 200009 | relogin | 是 | 永久 | 401 |
| 10 | 403（文案指向登录态） | relogin | 是 | 永久 | 401 |
| 11 | 403（其它） | transient | 是 | 否 | 403 |
| 12 | 429（非配额文案） | rate | 是 | 账号×模型 | 429 |
| 13 | 刷新冲突（200822） | transient | 是 | 否 | 502 |
| 14 | 5xx / Status 0 | transient | 是 | 否 | 502 |
| 15 | 兜底 | other | 否 | 否 | 400 |

### 业务码的两种形态

上游的 `code` 不只有数字：也会发 `"insufficient_points"` / `"rate_limit_exceeded"`
这类**字符串名字**。`code` 解成 `int` 会失败并留下 0，等于「没有错误码」，
所以解析要同时给出数字码与文本码（`internal/raccoon/chat.go` 的
`codeFromBodyEx`），分类侧按两者一起判（`raccoon.IsQuotaCode` /
`IsAuthCode` / `IsContentPolicy`）。流内错误帧同样如此：`streamFrame.Code`
用 `RawMessage`，否则一帧字符串码会被当成坏帧跳过，整条流退化成「上游返回空响应」。

流内错误帧**不再伪造 502**：HTTP 本来就是 200，伪造的 502 会让「积分耗尽」
掉进「上游波动，换号」分支，号永远不冷却。状态码留 0，由业务码与文案分类。

### 内容策略拦截

`blocked by security policy` / `unapproved channel` / `illegal api invocation`
是上游对**客户端模板句**的逐字拦截。账号本身健康（余额够、没限流、会话没死），
换号只是白扔另一个号的额度——同一份 body 换谁发都会被拦。所以归类为
`rejected`：不冷却、不换号、回 400。

### 第 1 行的实现要点（客户端断开）

判据是 `errors.Is(err, context.Canceled)` 顺着错误链能摸到 sentinel。上游流读到
一半被打断时，`raccoon.ChatStream` 会把它放在 `UpstreamError.Cause` 里并实现
`Unwrap`；**只把它写进 `Message` 是不够的**——文案能被人看到，但 `errors.Is`
认不出来，于是会掉进第 14 行「Status 0 → 上游波动」：给健康号记退避、流水记 502。

两种取消要分开，都看父 ctx：

| 现象 | 父 ctx | 归因 | 流水 |
|---|---|---|---|
| 用户在 App 里点停止、关页面、客户端自身超时 | 已取消 | 客户端断开 | 499 |
| 上游超过空闲阈值没出数据（我们自己的计时器取消） | 未取消 | 上游静默 | 502 |

`context.Canceled` 作为 Cause 只挂在「客户端断开」这一支上；空闲超时那一支不挂，
否则上游静默会被误判成客户端断开而跳过退避。

499 在面板上的口径：**既不算成功也不算失败**。调用日志单列一档「客户端取消」，
不染失败底色；「失败」计数、4xx 筛选、成功率、耗时分布都把它排除在外；
日聚合同样跳过它，不让它进请求数。

## 冷却时长

- **积分耗尽**：停到该账号下一次签到补分；拿不到签到计划时回退次日 04:00。
- **账号级（200108）与模型级（429）**：同一条阶梯 3 → 5 → 10 → 20… 分钟，封顶 24 小时；
  冷却结束 24 小时内再犯继续上一档，成功一次清零。
- **上游给了 `Retry-After` / `reset_time`**：取「阶梯」与「上游给的时长」中较长的一个，
  避免上游要 20 分钟、本地 3 分钟就放出来、到期再撞一次。
- **请求侧错误**（超长 / 过大 / Schema / 内容策略拦截）：永不计入冷却，也不换号。

## 签到与账单

每日签到不是「某个接口回 200 就算成功」：

1. `setting_info` 是每日积分的**发放触发器**（按天幂等）。通道顺序与余额查询一致
   （组织号 → v3 → v2）——v3 被网关整体拦截过一段时间（任何凭证都 403），
   只写死 v3 会让签到每天在第一步静默失败。
2. 积分账单（`/api/web/points/v2/bills`）核对**今日实际入账**，那才是权威结果；
   账单核对失败记为「未核实」，不谎报成功，也不据此断言「没发」。
3. 「今天」按北京时间（UTC+8）取日键，不跟容器时区。

## 会话与轮次标识

- `X-RACCOON-Session-ID` **跨轮不变**：优先由显式会话键
  （`X-Session-Id` / `conversation_id` / `prompt_cache_key` 等）派生，
  没有才退到首条 user 消息。每轮都换会让上游把同一段对话看成孤立请求。
- `X-RACCOON-Turn-ID` 只哈希到**最后一条 user 消息**为止：工具结果接在其后，
  属于同一轮，不构成新 turn。
- 目录里 `visible:false` 的模型保留在清单中（供显式映射），但不进 `/v1/models`。

### 缓存命中与账号粘性

上游的前缀缓存是**按账号隔离**的：同一段对话的每一轮如果换了号，缓存必然全 miss。
所以命中率的第一决定因素不是请求体，而是「这几轮有没有落到同一个号上」。

本地做两件事：

1. **会话粘性**：显式会话键（`X-Session-Id` / `X-Conversation-Id` / `Session-Id` /
   `session_key` / `session_id` / `conversation_id` / `prompt_cache_key`）→ 绑定账号，
   TTL 内复用。`prompt_cache_key` 是 OpenAI SDK 的官方缓存路由字段，发往上游前会被删掉，
   但本地拿它当会话键正是它的本意。
2. **兜底粘性**（`SESSION_AFFINITY_FALLBACK`，默认开）：客户端一个键都不发时，
   按**首条 user 消息**派生一个 `first-user:<文本>` 绑定键。大多数 SDK 默认不发会话键，
   不兜底的话每次请求都按负载均衡换号，上游缓存永远命中不了。
   代价是首条消息相同的不同对话会被当成同一段——这是刻意取舍，可在面板关掉。

提示（`pool.PickHints`）随每次选号调用传入，**不放在 `Pool` 字段上**：池是全局单例，
并发请求写同一个「本轮优先号」会互相覆盖。

上游 usage 里的缓存字段两种方言都认，解析后统一到 `raccoon.Usage`：

| 来源 | 字段 | 口径 |
|---|---|---|
| OpenAI | `prompt_tokens_details.cached_tokens` | `prompt_tokens` 已含缓存 |
| Anthropic | `cache_read_input_tokens` / `cache_creation_input_tokens` | `input_tokens` **不含**缓存 |

内部以 OpenAI 口径为准（输入含缓存），对外按各协议拆回去。上游没回传缓存字段时
标记 `CacheReported=false`，三条协议都**不输出**缓存字段——「不知道」不能写成「命中 0」。

## 对外透出

- 上游给了重置时刻时，错误响应带 `Retry-After: <秒>`。
- 号池暂时抽干时，会挑一个最早恢复的号实打一次，把上游真实错误交回调用方，
  而不是替换成「没有可用账号」。

## 外部渠道（AutoClaw / 码道 CodeArts）

这两家与上面三家不是同一套上游：凭据形态、请求签名、错误码都不同，所以各自
独立成包（`internal/autoclaw`、`internal/codearts`），只通过
`relay.ExtUpstream` 接口接入转发层。

| 项目 | AutoClaw | 码道 CodeArts |
|---|---|---|
| 鉴权 | `X-Authorization: Bearer <access_token>`（对话）；业务接口用 `Authorization` | 临时 AK/SK/security-token 做 SDK-HMAC-SHA256 签名 |
| 凭据刷新 | `refresh_token` 一次性，轮换后旧值作废 | STS 的 `refresh_token` 一次性 + DPoP 私钥 |
| 凭据落库 | `ext_token` / `ext_refresh_token` / `ext_extra`(JSON) / `ext_site` | 同上（`ext_extra` 存完整凭据含 DPoP 私钥） |
| 并发 | 跟随全局 | 每账号最多 3 个会话（`x-ot-session-id`） |
| 模型名 | `autoclaw/<routeModelId>`，如 `autoclaw/zai_auto` | `codearts/<model_id>`，如 `codearts/GLM-5.2` |
| 签到 | `autoclaw-task-complete` 的 `daily_signin`（先调 `user-info` 领活动） | `/v1/ops/delivery` → `claim` → `confirm` |
| 额度 | 纯积分余额（`wallets`） | 积分桶（`statistics/plugin`）+ 免费日额度 |

### 多副本下的续期

两家都要求「同一凭据的续期只被采纳一次」，否则后写者会把已轮换的
`refresh_token` 覆盖成作废值，账号直接掉登录。实现分三层：

1. 进程内按账号单飞（`sync.Mutex`）；
2. 写回时用库里的 `ext_rev` 做 CAS（`store.ReplaceExtCredential`），版本被别的
   副本改过就放弃写入；
3. CAS 失败时重读库，用对方写回的新凭据，而不是硬写自己的。

`ext_rev` 是账号表上新增的一列，随每次凭据写回 +1。

### 错误映射要点

- 带渠道前缀的模型名只在路由层使用，发给上游前会被剥掉；`/v1/models` 与流水
  保留带前缀的名字，客户端看到的模型名才稳定。
- AutoClaw 的 406 是「system 内容被拒」：`internal/autoclaw/body.go` 的
  `RewriteBody` 会补官方 system 前缀、改写其它 agent 的身份句、删掉
  `max_tokens` 与 `X-Harness-Type`。
- 码道的错误多半塞在 HTTP 200 的流里（`InferHub.*`），由 `embeddedErrorStatus`
  换算成 429 / 400 / 502；临时凭据过期（`APIG.0301/0307/0602`）会先续期一次再重发。

## 参考

上述规则参考了以下开源项目的思路：aigw-backend、Agent2API、AgentHub，
以及 magpie 社区插件 `@magpie-community/opencode-autoclaw-auth` 与
`@magpie-community/opencode-codearts-auth`（AutoClaw / 码道的协议细节）。
