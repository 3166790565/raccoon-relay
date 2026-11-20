# 发版流程

本目录存放每个版本的发布说明（`<tag>.md`），同时被两处使用：

- `scripts/release.sh tag <tag>` 用它作为 **git 注释 tag 的正文**；
- 发布后用它覆盖 GitHub Release 的自动生成说明：
  `gh release edit <tag> --notes-file docs/releases/<tag>.md`

## 标准流程

```bash
# 1. 发版前检查（只读，不改任何东西）
scripts/release.sh check v1.0.5

# 2. 写发布说明
cp docs/releases/v1.0.4.md docs/releases/v1.0.5.md   # 改成这一版的内容

# 3. 打 tag 并推送（内部会再跑一遍检查）
scripts/release.sh tag v1.0.5

# 4. 盯 CI，校验 Release 资产与 sha256
scripts/release.sh watch v1.0.5

# 5. 补发布说明（CHANGELOG 写完后）
gh release edit v1.0.5 --notes-file docs/releases/v1.0.5.md

# 6. 下载本平台发布版实跑一次
scripts/release.sh verify v1.0.5
```

## 检查项与它拦住的真实事故

`check` 的每一项都来自一次实际踩坑，不是凭空加的：

| 检查 | 拦住的事故 |
|---|---|
| 工作区干净 | tag 指向未提交的 HEAD，Release 与代码对不上 |
| 分支与远端同步 | 打 tag 时漏推提交，CI 构建的是旧代码 |
| 版本号未占用 | 重复 tag 导致 Release 覆盖 / CI 冲突 |
| `gofmt` / `app.js` 语法 | 面板 JS 缺括号，页面整个白屏 |
| **无 `testdata` 跑测试** | 测试硬依赖被 `.gitignore` 的样例，本地绿、CI 红（v1.0.4） |
| 六平台交叉编译 | 发布矩阵里某个架构编不过，Release 缺资产 |
| **`time/tzdata` 内嵌** | Windows/macOS 发布版启动即报 `TZ 非法`（v1.0.4） |
| 资产名与更新器一致 | 改名后「一键更新」找不到安装包 |
| `checksums.txt` | 缺校验文件时更新器拒绝安装（设计如此） |
| 发布说明存在 | Release 只有一行 compare 链接（v1.0.4 补过） |
| store 层改动 | 有 schema 变更却没验证旧库升级 |

## 发版前的人工确认（脚本查不到）

- **旧库升级**：拉一个上一版本创建的 `relay.db`，用新二进制起一次，确认能正常打开、
  账号/冷却/设置都还在。
- **Docker 实跑**：`docker pull bxacc123/raccoon-relay:<tag>`，起容器，确认 `/health` 与面板可用。
  一键更新这条链路只有在容器里才能完整验证。
- **发布版实跑**：至少在你当前平台上跑一次 `scripts/release.sh verify <tag>`。
- **一键更新跨版本**：如果这版改了 `internal/updater`，用**上一个版本**去更新到这一版，
  而不是用这一版更新到自己。
- **面板冒烟**：登录 → 号池 → 设置各项保存 → 重启 → 复查设置是否还在。

## 版本号约定

- 正式版 `vX.Y.Z`：会更新 Docker 的 `latest`，并创建/更新 GitHub Release。
- 预发布 `vX.Y.Z-rcN`：**不**覆盖 `latest`（CI 里 `!contains(github.ref_name, '-')` 控制），
  但同样会建 Release 和二进制。用这个先验证，别直接打正式 tag。

## 发布说明怎么写

按主题分节，不要逐条罗列 commit：

1. 新增功能（号池/授权/面板/运维分组）
2. 修复（写**用户能感知的现象**，不是「修了 xxx 函数」）
3. 升级说明（是否需要手工操作、环境变量是否兼容、镜像与裸机差异）

模板见 `v1.0.4.md`。
