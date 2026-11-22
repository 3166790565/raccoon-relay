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
| 6 | 敏感词拒答（100041 / 100042） | rejected | 否 | 否 | 400 |
| 7 | 积分不足（1000007 / 402） | quota | 是 | 整号至下次签到 | 402 |
| 8 | 账号异常（200108） | account_abnormal | 是 | 账号级阶梯 | 429 |
| 9 | 401 / Auth 标记 | relogin | 是 | 永久 | 401 |
| 10 | 403（文案指向登录态） | relogin | 是 | 永久 | 401 |
| 11 | 403（其它） | transient | 是 | 否 | 403 |
| 12 | 429 | rate | 是 | 账号×模型 | 429 |
| 13 | 刷新冲突（200822） | transient | 是 | 否 | 502 |
| 14 | 5xx / Status 0 | transient | 是 | 否 | 502 |
| 15 | 兜底 | other | 否 | 否 | 400 |

## 冷却时长

- **积分耗尽**：停到该账号下一次签到补分；拿不到签到计划时回退次日 04:00。
- **账号级（200108）与模型级（429）**：同一条阶梯 3 → 5 → 10 → 20… 分钟，封顶 24 小时；
  冷却结束 24 小时内再犯继续上一档，成功一次清零。
- **上游给了 `Retry-After` / `reset_time`**：取「阶梯」与「上游给的时长」中较长的一个，
  避免上游要 20 分钟、本地 3 分钟就放出来、到期再撞一次。
- **请求侧错误**（超长 / 过大 / Schema）：永不计入冷却，也不换号。

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
