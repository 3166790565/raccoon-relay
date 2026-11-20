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

也兼容 agent2api 的桌面导出（`{"version":2,"exportedAt":...,"accounts":[{"provider":...}]}`）。
这种包是多供应商混装，导入时只保留小浣熊账号（`provider` 为 `raccoon`；缺失时按 token 的
`iss = xiaohuanxiong.com/iam` 判别），其余 qoder / trae / codearts 等会被自动过滤，
面板会提示过滤掉多少条。agent2api 的 token 放在 `pluginSecret.accessToken`，里面没有
独立的 `refresh_token` 字段；小浣熊这两个本来就是同一枚凭据，导入时直接用 `accessToken`
回填 `refreshToken`，所以导入的号一样能自动续期。

---

## 号池

每个请求按策略选号，失败自动换号重试（默认 3 次）。面板「设置」页可切换，改完立即生效。

| 策略 | 排序 |
|---|---|
| `load_balance`（默认，负载均衡） | 在途请求最少 → 最久未用 → 积分多者 → 先到期者 |
| `round_robin`（轮询） | 最久未被选中的优先，请求均匀摊到每个号上 |

> 旧版本里的 `optimal` / `credit_first` / `manual` / `expire_first` 升级后自动按「负载均衡」处理，
> 无需改环境变量。面板里改的选号策略、重试、签到时间、冷却阈值、续期与日志设置都会落库，
> 重启后仍然生效；环境变量只提供首次启动的默认值。

失败分级：

| 上游返回 | 处理 |
|---|---|
| 402 积分不足 | `exhausted`，冷却到次日 04:00（此时**不会**换号重试） |
| 429 限流 | **只冷却「这个号 × 这个模型」**，账号保持在线；冷却阶梯 3 → 5 → 10 分钟，之后逐次翻倍（封顶 24 小时），该模型再次调用成功即清零 |
| 5xx / 网络错误 | `cooling` 10 分钟 |
| 401 / 令牌过期 | 先用 `refresh_token` 强刷一次并重放；刷得动就继续用，刷不动才判 `relogin` |
| 403 风控/权限 | `cooling` 2 小时并换号。403 **不等于**凭证过期，不会把号打成 `relogin` |
| 凭证失效（refresh 也失败） | `relogin`，等重新导入凭据 |
| 模型级错误（6004 / 11102） | 只冷却「这个号 × 这个模型」，换模型仍可用 |

### 积分阈值冷却

面板「设置 → 冷却与积分阈值」可开启：

| 项 | 说明 |
|---|---|
| 低于该积分暂停 | 已知余额 ≤ 该值的账号暂停选号（`exhausted`，无固定到期时间） |
| 达到该积分解除 | 余额 ≥ 该值时自动恢复在线 |

余额由签到与手动「查余额」异步更新，写入成功后立即按阈值重算，另有每分钟巡检兜底。
规则开启时「解除阈值」必须**大于**「暂停阈值」（相等会让状态在阈值上反复翻转）。
402 造成的硬耗尽（带次日 04:00 到期时间）不受此规则影响，到点由冷却巡检自动放回在线。
「429 按账号 × 模型冷却」开关关闭后，429 退化为整号冷却 60 秒。

### 号池表格

面板「号池」页是一张密集的账号表，每行依次是：勾选、序号、账号（名称 + uid）、启用开关、
状态、限流、有效期、余额、操作。顶部可以按「启用/停用」「正常/已限流」和关键字（账号名 /
uid / 备注）筛选，筛选结果可一键全选后批量启用、停用、签到、解除冷却或删除。

**限流**列要分开看两级：

- 账号级冷却（`cooling` / `exhausted` / `relogin`）：显示原因和剩余时间；
- 模型级罚单（429 只罚「账号 × 模型」）：显示「N 个模型」，点开可看到每个模型的原因、
  恢复倒计时，也可以单条「清除」或「全部解除」。手动解除会同时清空该组合的失败链，
  下次再 429 从 3 分钟重新算。

账号整号状态显示「正常」时，模型级罚单仍然可能拦着某个模型——这正是这一列存在的意义。

「查询余额」未勾选账号时查全部启用账号，勾选后只查选中的那批；查询逐个串行执行，
避免同一出口 IP 并发查余额触发风控，单个号失败只标注该行，不影响其它号。

对应接口：`POST /api/accounts/balances`（`{"ids":[...]}` 可省略表示全部启用账号）、
`POST /api/accounts/batch`（`{"action":"enable|disable|delete|checkin|uncool","ids":[...]}`）、
`POST /api/accounts/{id}/uncool`（带 `{"model":"..."}` 只解该模型，否则解账号级冷却）。

### 清理失效账号

面板「号池」页的「清理失效账号」按钮会删掉所有**救不回来**的号：

| 口径 | 说明 |
|---|---|
| `relogin` | refresh_token 也刷不动，只能重新登录拿新凭据 |
| `expires_at` 已过 | 订阅到期，凭据能用也不会再产出额度 |

`exhausted`（积分耗尽）和 `cooling`（临时冷却）**不算**：它们会自己恢复，删了就回不来。

点按钮会先拉一份预览清单列给你确认；确认后只提交这份 id，后端删除前会再按最新状态复核，
预览之后被「刷新」救回来的号不会被误删。删除在单个事务里完成，连带清掉该号的
`model_cooldowns` 与 `checkin_history`，不会留下孤儿行。

对应接口：`GET /api/accounts/dead`（预览）、`POST /api/accounts/dead/purge`
（必须带 `{"ids":[...]}`；空请求体会被拒绝，避免误删）。

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
- 每轮顺带刷一次余额。余额走 `GET office/v2/setting_info` 的 `available_points`
  （网页版头部同源）；`office/v3/setting_info` 自 2026-11 起被网关整体拦截（403，
  响应头 `X-TT-System-Error: 3`），留作第一回退；旧 `points/v1/balance` 已下线，只作最后兜底。
- 启动时会补跑：错过窗口不会漏签。

面板「号池」页全选后「批量操作 → 批量签到」可以手动触发一轮，也可以在账号行菜单里单独签到。

---

## 配置

全部走环境变量，见 `.env.example`。几个值得注意的：

- `SECRET_KEY`：换了这个值，库里已加密的 token 就解不开，需要重新导入。
- `REFRESH_WINDOW_SEC`（默认 300）：转发路径上，access 只在临期 5 分钟内才刷。**不要调大**——
  抢在官方客户端前面刷新会把 `refresh_token` 轮换掉，两边同时掉登录。
- `RENEW_WINDOW_SEC`（默认 259200 = 3 天）/ `RENEW_INTERVAL_MIN`（默认 360）/
  `RENEW_GAP_MS`（默认 1000）：后台主动续期。小浣熊的 token 是 30 天有效期的 JWT；
  转发路径只在有请求打到某个号时才刷新，冷门号可能几周不被选中，到期后直接从号池消失。
  主动续期每 6 小时扫一遍，把 3 天内到期的号统一刷新，续期成功还会把被误判成
  `relogin` 的号拉回在线。设 `RENEW_WINDOW_SEC=0` 可关闭。
  这三项也可以在面板「设置 → 主动续期」里改，**保存后立即热生效**（循环按新参数重排，
  不需要重启），并持久化到数据库；环境变量只提供首次启动的默认值。同一张卡片上还有
  「立即续期一次」按钮，手动触发一轮并返回检查/续期/失败/恢复的条数。
- `OAUTH_REDIRECT_BASE`：配了才会启用 `/oauth/callback` 回环回调路径。
  取决于上游是否接受自定义 `redirect_uri`，实测能用再开；不能用并不影响粘贴授权码那条路。

---

## 日志管理

运行日志（进程输出）和调用流水（`usage_logs` / `checkin_history`）分开管理，都能在
面板「设置 → 日志管理」里改，保存后立即生效并持久化到数据库；环境变量只提供首次启动的默认值。

**运行日志文件**

- 写到 `LOG_DIR/relay.log`（默认 `DATA_DIR/logs`），同时保留标准错误输出，容器 `logs` 照旧可看。
- 单文件超过 `LOG_MAX_SIZE_MB` 或跨天时自动轮转成 `relay-YYYYMMDD-HHMMSS.log`。
- 旧归档按 `LOG_FILE_RETAIN_DAYS` 和 `LOG_MAX_FILES` 中更严格的一条清理：
  过期天数的先删，剩下的再按数量从最旧的删。
- 面板「运行日志」页可以看文件清单、大小、下载单个文件，也能点「立即清理」。

**调用流水**

- `LOG_DB_RETAIN_DAYS`（默认 30）控制 `usage_logs` 与 `checkin_history` 保留天数。
- 清理按时间戳批量删除（每批 1000 条），不会一条 `DELETE` 卡住整个 SQLite 写锁。
- 设为 `0` 表示不按天数清理数据库，只清理日志文件。

**自动清理**

- `LOG_CLEAN_ENABLE` + `LOG_CLEAN_INTERVAL_MINUTES`（默认每小时）控制后台清理节奏。
- 启动时会先跑一次清理，避免停机很久后旧文件堆积。
- 手动「立即清理」不受开关影响，随时可用。

**调用日志页**

- 服务端分页：`GET /api/logs?page=&pageSize=`（每页最多 500 条），同一次查询顺带返回
  总数与失败数，历史流水再多也不会把浏览器拖死。
- 筛选都下推到 SQL：时间窗（`since`/`until`，毫秒）、`status`（`ok`/`fail`/`4xx`/`5xx`）、
  `account`、`model`、`api`，以及模糊搜索 `q`（模型 / 错误 / 接口 / 账号名或 uid）。
- `GET /api/logs/facets` 返回近 30 天出现过的模型与接口，供筛选下拉使用，结果缓存 30 秒。
- 「自动刷新」可按 5/10/30/60 秒轮询；只在停留在本页且标签页可见时生效，切走或最小化即停。
- 「列设置」可隐藏不常看的列，选择存在浏览器本地。

> 注意：`usage_logs` 是面板「调用日志」的数据源。保留天数设得太小会让统计窗口变短，
> 但不会影响号池与签到本身。

---

## 一键更新

面板「版本更新」页可以检查并安装新版本，不需要重新拉镜像或手动替换文件：

1. 进入 `/admin` → 「版本更新」→ 「检查更新」。默认从本项目 GitHub Release 的
   `relay-<os>-<arch>[.exe]` 资产取当前平台安装包。
2. 点「一键更新」：下载 → 校验 `checksums.txt` 里的 sha256 → 原子替换 → 重启。
3. 重启前会把旧版本留在 `relay.bak`，面板上有「回滚」按钮。

**Docker 部署**：新二进制安装在数据卷 `/data/bin/relay`，容器入口脚本会优先启动它。
这样重启、重建容器后更新都还在；想回到镜像自带的版本，删掉 `/data/bin/relay` 再重启。
容器内不需要额外权限，也不需要挂载 Docker socket。

**裸机部署**：替换的是当前可执行文件。Linux/macOS 用 `exec` 无缝重启；Windows 无法原地
重启，安装完成后按面板提示手动启动即可（新文件已经替换好）。

限制与注意：

- 只认 GitHub Release 资产并对 sha256 强制校验，缺 `checksums.txt` 的版本会被拒绝安装。
- GitHub API 匿名调用有频率限制；被墙或限流时设 `GITHUB_TOKEN`，或用
  `UPDATE_API_BASE` / `UPDATE_DOWNLOAD_BASE` 走镜像。
- 更新失败不会动当前版本；更新成功但新版本起不来，用「回滚」或删掉数据卷里的
  `bin/relay` 重启即可。

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

**上游 403 / 面板显示「需重登」**
403 是风控、权限或设备指纹问题，不等于账号过期。现在遇到 403 会做短冷却并换号，
不会再把号永久打成 `relogin`。如果面板上已经有号停在「需重登」、但你确定 token 还有效，
点该行的「刷新」：刷新成功会自动把它恢复成 `online`。

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

---

## 发版

发版流程固定在 [`docs/releases/README.md`](docs/releases/README.md)，用脚本执行：

```bash
scripts/release.sh check v1.0.5    # 发版前检查（只读）
scripts/release.sh tag v1.0.5      # 打 tag 并推送，触发 CI
scripts/release.sh watch v1.0.5    # 等 CI、校验 Release 资产与 sha256
scripts/release.sh verify v1.0.5   # 下载本平台发布版实跑一次
```

发布说明存放在 `docs/releases/<tag>.md`，同时用作 tag 正文与 GitHub Release 说明。
`check` 会模拟 CI 环境（挪开被 gitignore 的 `internal/creds/testdata/`）跑一遍全量测试，
并逐项检查六平台交叉编译、`time/tzdata` 内嵌、更新器资产名与 CI 产物名是否一致。
每一项都对应一次实际踩过的坑，说明见该文档。
