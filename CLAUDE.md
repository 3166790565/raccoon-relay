# 小浣熊反代（raccoon-relay）

Go 服务：把多个「小浣熊」账号聚合成 OpenAI / Anthropic / Responses 兼容网关，
带管理面板（`/admin`）与运行期设置页。数据面与管理面同端口。

## 项目规则（必须遵守）

### 1. 新增配置项必须在 WebUI 可改

**任何新增的可调参数，只要环境变量能配，就必须同时能在面板里改。** 这是硬性要求，
不接受「先加环境变量、面板以后再说」。

具体做法（照 `internal/admin/admin.go` 里已有的卡片来抄）：

1. `internal/config/config.go`：加字段 + `Load()` 里读环境变量作为**首次启动的默认值**。
2. `internal/admin/admin.go`：
   - `settingsBody` 加指针字段（只处理显式提交的项）；
   - `applyXxxSettings(ctx, body)` 做校验 + 热更新 + `st.SetSetting` 持久化；
   - `applyStoredXxxSettings(ctx)` 在 `ApplyStoredSettings` 里调用，重启后盖回库里的值；
   - `xxxSettings()` 返回当前**生效值**，`handleGetSettings` 里合进响应；
   - `handlePutSettings` 里调用 `applyXxxSettings`，校验失败整体拒绝。
3. `internal/webui/index.html`：设置页加一张卡片或一组字段。
4. `internal/webui/app.js`：`loadSettings()` 回填、单独的保存按钮（只提交本卡片字段）。
5. `.env.example`：加注释说明「面板可改，这里只是默认值」。
6. 补 `internal/admin/*_settings_test.go`：热更新、持久化、非法值拒绝、重启恢复。
7. 如果值要热生效，还得有一个进程级的「设置入口」（如 `raccoon.SetFingerprint`），
   而不是只改 `cfg` 字段——`cfg` 在启动后被多处读取，改了也不一定生效。

注意两类边界：

- **空值是合法配置**的项（如 UA 留空 = 自动派生）不能用 `GetSetting` 读，
  它会把「存的是空串」和「键不存在」混为一谈；用 `GetSettingOK`。
- 派生值不要存库。存原始值，读取时派生（见 `Fingerprint.ChatUA()`），
  否则改了一个参数，派生结果仍停在上一次的旧值上。

### 2. 环境变量只提供默认值

面板保存过的值优先于环境变量，重启后仍然生效。这是全项目一致的约定，
新增配置必须遵守，不能出现「面板改了、重启又回去了」。

### 3. 不要动上游凭据的刷新时机

`REFRESH_WINDOW_SEC` 默认 300 秒，**不要调大**：抢在官方客户端前面刷新会把
`refresh_token` 轮换掉，两边同时掉登录。

### 4. 区分「冷却」与「退避」

- **冷却**（`pool.CooldownBook`）：写账号状态（`cooling`/`exhausted`/`relogin`），
  面板上会显示成限流或停用，原因是账号本身有问题（200108、429、积分耗尽）。
- **退避**（`pool.MarkBackoff`）：只让调度层在窗口内跳过它，**不改账号状态**，
  原因通常是上游波动（403、5xx、网络）。调用成功即清空。

新增失败分类时先想清楚属于哪一种：把波动类失败写进冷却，会凭空少一个可用号。

### 5. 并发相关的改动要考虑多副本

在途租约、退避、会话绑定都落在数据库里，靠 UPDATE 的 CAS 或主键冲突保证原子性。
不要在进程内 map 里做「同一账号只能有一个请求」这类判断——多副本下会失效。
进程内 map 只能做缓存/节流（如 `leaseState` 只记住「我写进去的到期时刻」用于 CAS）。

## 目录速览

| 目录 | 职责 |
|---|---|
| `cmd/relay` | 装配与启动 |
| `internal/relay` | 数据面：三条协议、转发引擎、并发闸门、续期 |
| `internal/raccoon` | 上游客户端：头集、错误分类、流式解析、指纹 |
| `internal/pool` | 号池：选号、冷却（账号级 / 模型级）、在途租约、失败退避 |
| `internal/scheduler` | 自动签到、新号初始化 |
| `internal/admin` | 管理面 API 与设置持久化 |
| `internal/webui` | 单页面板（`index.html` + `app.js`，嵌入二进制） |
| `internal/store` | SQLite / Postgres 存储层 |

## 测试

```bash
go test ./...
```

设置类改动至少要覆盖：热更新生效、持久化、非法值整体拒绝且不改动现有值、重启后恢复。