---
name: release
description: 小浣熊反代（raccoon-relay）发版流程。当用户说要发版、打 tag、发布新版本、出 Release、收尾提交时使用。包含发版前检查、版本号、发布说明、CI 校验和发布后验证的完整步骤。
---

# 小浣熊反代发版

本项目的发版链路是：提交 → 推 master → 打 `vX.Y.Z` tag → CI 构建镜像与 6 平台二进制 →
GitHub Release → 补发布说明 → 下载实跑。每一步都有踩过的坑，按顺序做，不要跳。

## 先做的事

1. `git status` —— 确认工作区里有哪些改动。**可能有其它对话遗留的未提交改动**
   （例如 `internal/raccoon/client.go` 正在被另一个会话改余额接口），
   不要替它们写代码，但要决定：等它们完成，还是只提交本次相关的文件。
2. 读 `docs/releases/README.md` —— 那是流程的权威说明，本 skill 只是入口。
3. 确认版本号：`git tag -l 'v*' --sort=-v:refname | head -3`。

## 步骤

### 1. 发版前检查

```bash
VERSION=vX.Y.Z bash scripts/release.sh check
```

全部通过才继续。任何 ✗ 都要先修；⚠️ 要看情况（发布说明缺失、store 层有改动属于需要人工确认的项）。

这个脚本会**模拟 CI**：把 `internal/creds/testdata/` 临时挪开再跑测试。
因为该目录被 `.gitignore` 排除，CI 上没有它——本地绿不代表 CI 绿。

### 2. 写发布说明

```bash
cp docs/releases/v1.0.4.md docs/releases/vX.Y.Z.md
```

按主题分节：新增功能 / 修复 / 升级说明。修复项写**用户能感知的现象**。
不要写「修了 xxx 函数」，要写「模型冷却阶梯永远停在 3 分钟」。

### 3. 打 tag

```bash
bash scripts/release.sh tag vX.Y.Z
```

它会再跑一遍检查，然后用 `docs/releases/vX.Y.Z.md` 作为注释 tag 的正文并推送。
CI 会自动构建并创建 Release。

**如果 CI 失败**：
- 看 `gh run view <id> --log-failed`。
- 失败原因若是代码问题 → 修复、提交、推送，然后删掉 tag 重新打：
  `git tag -d vX.Y.Z; git push origin :refs/tags/vX.Y.Z`
- 如果 Release 已经创建（带坏二进制）→ 先 `gh release delete vX.Y.Z --yes` 再重打，
  否则新资产会以 `--clobber` 覆盖旧资产，但旧的 Release 说明和资产列表会混在一起。
- 不要在 CI 红的 commit 上留 tag。

### 4. 盯 CI 并校验资产

```bash
bash scripts/release.sh watch vX.Y.Z
```

它会等 CI 完成、检查 7 个必需资产、用 `checksums.txt` 校验下载内容，并提醒补发布说明。

### 5. 补发布说明

```bash
gh release edit vX.Y.Z --notes-file docs/releases/vX.Y.Z.md
```

CI 生成的默认说明只有一行 compare 链接，必须覆盖。

### 6. 实跑发布版

```bash
bash scripts/release.sh verify vX.Y.Z
```

下载当前平台二进制、核对 sha256、启动、检查 `/health` 与版本号。
这一步能拦住「编译通过但启动即崩」的问题（v1.0.4 就是 `TZ 非法`）。

### 7. 提交发版工具本身

`scripts/release.sh`、`docs/releases/*.md`、`.claude/skills/release/` 都是仓库的一部分，
改动它们之后要单独提交，不要跟业务代码混在一个 commit 里。

## 这个项目特有的陷阱

- **时区数据**：`cmd/relay/main.go` 必须有 `_ "time/tzdata"`。没有它，交叉编译出来的
  Windows/macOS 二进制在没有系统 IANA 数据的机器上会 `TZ 非法` 起不来。`check` 会检查。
- **更新器与 CI 产物名**：`internal/updater/updater.go` 的 `assetName()` 必须与
  `.github/workflows/docker.yml` 的 `relay-${GOOS}-${GOARCH}${suffix}` 完全一致，
  改文件名要两边同步。
- **预发布 tag**：`vX.Y.Z-rcN` 不会覆盖 Docker `latest`，但会建 Release。
  先在 rc 上验证，别直接打正式 tag。
- **Docker 一键更新**：只有在容器里才能完整验证（安装到 `/data/bin/relay`，
  重启时 exec 该路径）。裸机 `verify` 覆盖不到这条链路。
- **`-race` 跑不了**：本机没有 gcc，`CGO_ENABLED=1` 不可用。并发正确性靠单飞锁设计和用例，
  不要声称跑过竞态检测。
- **GitHub API 限流**：本机匿名请求经常 403。`/api/update` 显示限流错误是环境问题，
  不是代码 bug。服务器上配 `GITHUB_TOKEN`。

## 不要做的事

- 不要在 `check` 未通过时打 tag。
- 不要为了「让 CI 变绿」而删除或跳过测试；如果测试确实依赖本地样例，改成「缺样例则跳过」，
  参照 `internal/creds/importexport_test.go` 里其它用例的写法。
- 不要替其它对话改的文件写代码。如果它们阻塞了发版，告诉用户。
- 不要在 Release 说明里写「已修复全部已知问题」这类无法验证的话。
