# 小浣熊反代 · Raccoon Relay

把商汤小浣熊（xiaohuanxiong.com）的对话能力包成一个自托管的 OpenAI / Anthropic 兼容网关。
单二进制、单端口、纯 Go、无 cgo，只做三件事：**反代对话、号池轮换、自动签到**。

> 友链：[LINUX DO](https://linux.do/) —— 本项目在 LINUX DO 社区推广

---

## 快速开始

### 直接拉镜像部署（推荐）

```bash
cp .env.example .env
# 至少填这两项（都是随机串即可）：
#   ADMIN_TOKEN=<自己定一个>
#   SECRET_KEY=<自己定一个，别改>
docker compose up -d
docker compose logs relay
```

默认从 Docker Hub 拉 `bxacc123/raccoon-relay:latest`。指定版本用 `IMAGE_TAG`：

```bash
IMAGE_TAG=2026.11.13 docker compose up -d
```

### 从源码构建

```bash
docker compose -f docker-compose.yml -f docker-compose.build.yml up -d --build
```

日志里会打印两样东西（都只打印一次）：

```
已生成初始 API Key（只显示这一次）: rc-xxxxxxxx...
ADMIN_TOKEN: xxxxxxxx...
```

浏览器打开 `http://<host>:8080/admin`，用 `ADMIN_TOKEN` 登录。
把 API Key 配到客户端，接口地址填 `http://<host>:8080/v1`。

不想用 Docker：

```bash
go build -o relay ./cmd/relay
DATA_DIR=./data SECRET_KEY=dev ADMIN_TOKEN=dev ./relay
```

---

## 对接客户端

| 协议 | 端点 | 说明 |
|---|---|---|
| OpenAI | `POST /v1/chat/completions` | 支持流式与非流式，`reasoning_content` 原样透出 |
| OpenAI | `GET /v1/models` | 从上游 `model_catalog` 拉取，缓存 10 分钟 |
| Anthropic | `POST /v1/messages` | 含 `thinking` / `tool_use` 双向转换 |
| Anthropic | `POST /v1/messages/count_tokens` | **本地粗估**，响应里标注 `estimated: true` |
| Responses | `POST /v1/responses` | 支持 `input` 字符串或 item 数组 |

鉴权：`Authorization: Bearer rc-...` 或 `x-api-key: rc-...`
（`GET /health` 免鉴权，给健康检查用。）

---

## 加账号

### 方式一：粘贴授权码（推荐）

1. 面板「添加账号 → 粘贴授权码」点「打开小浣熊登录页」。
2. 浏览器里完成登录，收尾会跳到 `office-raccoon://auth/callback?code=…`：
   - **没装桌面客户端**：浏览器提示"没有可用应用"，地址栏里就是完整链接，整段复制。
   - **装了桌面客户端**：深链会被客户端吃掉，授权码会被它消费掉（再兑换会报 `200035`）。
     此时用无痕窗口重新走一遍，或者直接用手填令牌。
3. 把整段链接粘进面板，点「兑换并添加」。

后端做的是纯 HTTP：`POST /api/web/auth/v1/login_with_authorization_code` 换 token →
解 JWT `sub` 取 uid → 派生并绑定设备指纹 → 入池。容器只负责兑换，不参与回调。

### 方式二：手填令牌

从桌面客户端的 `~/.box-agent/config/auth.json` 里拷三个键：

```json
{
  "access_token": "eyJhbGciOi...",
  "refresh_token": "eyJhbGciOi...",
  "office_identity": ""
}
```

填进面板「手填令牌」。`access_token` 必须是合法 JWT 且带 `sub` 声明，否则会被拒绝——
`iss` 是所有账号共用的签发方常量，拿它当 uid 会让整池塌成一个号。

### 方式三：导入 JSON

直接导入桌面版导出的凭据包：

```json
{
  "format": "agenthub-credentials",
  "channel": "raccoon",
  "accounts": [
    { "channel": "raccoon", "uid": "pu-...", "token": "...", "refreshToken": "...", "meta": {...} }
  ]
}
```

按 `uid` 去重：已存在的账号合并凭据与 `meta`（保留 `onboardingGrants` / `checkin` / `deviceId`），不会重复建号。
导出走同一格式，可以直接回灌桌面版。

---

## 号池

每个请求按策略选号，失败自动换号重试（默认 3 次）。

| 策略 | 排序 |
|---|---|
| `optimal`（默认） | 免费额度 → 有效到期 → 确认永久 → 期限未知；同档积分多者优先 |
| `credit_first` | 积分多的优先 |
| `round_robin` | 最久未用的优先 |
| `expire_first` | 先到期的优先 |
| `manual` | 按 `priority` 字段 |

失败分级：

| 上游返回 | 处理 |
|---|---|
| 402 积分不足 | `exhausted`，冷却到次日 04:00（此时**不会**换号重试） |
| 429 限流 | `cooling` 60 秒（有 `Retry-After` 就按它） |
| 5xx / 网络错误 | `cooling` 10 分钟 |
| 401 且刷新失败 | `relogin`，等重新导入凭据 |
| 风控拦截 | `cooling` 2 小时 |
| 模型级错误 | 只冷却「这个号 × 这个模型」，换模型仍可用 |

已经向客户端写出内容之后不再换号重试，避免拼出半截回复。

---

## 自动签到

小浣熊的每日积分是**两步懒结算**：先 `GET setting_info` 激活当日 Web 会话，再
`POST desktop/v1/login/points/grant` 领取。少第一步会一直拿到 `granted:false` 且积分不落账。

- 计划时刻 = `CHECKIN_HOUR:CHECKIN_MINUTE` + 按 uid 派生的 `0~CHECKIN_JITTER_MINUTES` 分钟抖动。
  同一账号每天固定、重启不变，不同账号自然错开。
- `granted:true`（本次领到）和 `granted:false`（今日已领过）**都算当日完成**，都写 `last_checkin_day`。
- 401 → 标记 `relogin`，当日不再重试；5xx → 不标记，下一轮自然重试；同号当日连续失败 3 次 → 熔断。
- 账号之间串行，间隔 800~2000ms 随机，防风控。
- 顺带领一次性首登奖（桌面端 + 手机端两条互不相通的端点各领一次），台账存账号 `meta.onboardingGrants`；
  端点返回 404/410 就记 `-1` 永久停试。
- 每轮顺带刷一次余额。
- 启动时会补跑：错过窗口不会漏签。

面板上可以「全部签到」手动触发一轮，也可以在账号行上单独签到。

---

## 配置

全部走环境变量，见 `.env.example`。几个值得注意的：

- `SECRET_KEY`：换了这个值，库里已加密的 token 就解不开，需要重新导入。
- `REFRESH_WINDOW_SEC`（默认 300）：access 只在临期 5 分钟内才刷。**不要调大**——
  抢在官方客户端前面刷新会把 `refresh_token` 轮换掉，两边同时掉登录。
- `OAUTH_REDIRECT_BASE`：配了才会启用 `/oauth/callback` 回环回调路径。
  取决于上游是否接受自定义 `redirect_uri`，实测能用再开；不能用并不影响粘贴授权码那条路。

---

## 数据

`DATA_DIR` 下一个 `relay.db`（SQLite）：

| 表 | 内容 |
|---|---|
| `accounts` | 账号、凭据（加密）、状态、冷却、签到台账 |
| `api_keys` | 数据面 Key（只存 sha256） |
| `model_cooldowns` | 模型级负缓存 |
| `checkin_history` | 每次签到的结果 |
| `usage_logs` | 调用流水 |

备份直接拷 `data/` 目录即可。注意里面是加密后的凭据，`SECRET_KEY` 丢了就恢复不了。

---

## 常见问题

**401 / 掉登录**
`refresh_token` 是单副本、用后轮换的。同一个号在多处（桌面客户端、另一台机器）同时刷新，
先刷的那次会把后刷的那份作废。对策：一个号只在一处用；已经在桌面端登录着的号，别再拿它的凭据到处跑。

**200035**
授权码是一次性的，已经被消费过了（通常是桌面客户端抢走了深链）。回授权页重新登录取新码。

**积分耗尽 / 402**
号的钱花完了。面板上「查余额」确认，换个号或者充值。冷却到次日 04:00 才会自动复活。

**面板打不开**
检查 `ADMIN_TOKEN` 是否填了；没填时它在启动日志里随机生成。端口默认只绑 `127.0.0.1`，
要从外网访问就改 `docker-compose.yml` 的端口映射，并在前面挂一层 HTTPS 反代。

**`数据库初始化失败: 建表失败: unable to open database file (14)`**
数据目录写不进去。容器以 uid 10001（`relay`）运行，而 bind mount 的宿主目录通常属于
`root` 或你自己的账号，写权限对不上。

v1.0.1 起镜像内置入口脚本，启动时会自动修正 `/data` 的属主，正常情况下不用管。
如果仍然报错（宿主文件系统不支持改属主，比如某些 NFS 卷），在宿主机执行：

```bash
sudo chown -R 10001:10001 ./data
# 或者文件系统不支持改属主时：
sudo chmod -R 777 ./data
```

v1.0.0 及更早的镜像没有入口脚本，必须先用上面的命令手工修权限。

---

## 开发

```bash
go test ./...        # 单元 + 离线端到端（假上游，不烧积分）
go vet ./...
```

真实上游的联调会消耗积分，别放进 CI。

### 目录

```
cmd/relay/           启动与装配
internal/config/     环境变量
internal/store/      SQLite + 凭据加密
internal/raccoon/    上游协议（端点、头、JWT、思考档位归一、SSE）
internal/pool/       选号与冷却
internal/relay/      数据面三协议转换 + 换号重试
internal/login/      授权码兑换 / 手填令牌
internal/creds/      agenthub-credentials 导入导出
internal/scheduler/  自动签到
internal/admin/      管理面 REST
internal/webui/      内嵌单页面板
```
