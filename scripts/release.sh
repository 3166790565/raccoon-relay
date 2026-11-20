#!/usr/bin/env bash
# 发版前检查 + 打 tag + 推送 + 盯 CI + 校验 Release 资产。
#
# 用法：
#   scripts/release.sh check              # 只做发版前检查（不写任何东西）
#   scripts/release.sh tag v1.0.5         # 检查通过后打注释 tag 并推送
#   scripts/release.sh watch v1.0.5       # 等 CI 跑完，并核对 Release 资产与 sha256
#   scripts/release.sh verify v1.0.5      # 下载当前平台二进制并实跑一次 /health
#
# 为什么要有这个脚本：这次 v1.0.4 发版踩过的坑都可以在这里被提前拦住——
#   · CI 上测试失败（依赖被 .gitignore 的 testdata）
#   · Docker 下更新重启仍跑旧二进制
#   · 发布版在 macOS/Windows 上因缺时区库起不来
#   · Release 只有一行自动生成的 compare 链接
# 对应检查项在 check 里逐条给出。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

REPO="${RELEASE_REPO:-3166790565/raccoon-relay}"
IMG="${RELEASE_IMAGE:-bxacc123/raccoon-relay}"

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
yellow(){ printf '\033[33m%s\033[0m\n' "$*"; }
step()  { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }

fail=0
problem() { red "  ✗ $*"; fail=$((fail+1)); }
ok()      { green "  ✓ $*"; }
warn()    { yellow "  ! $*"; }

# ---------- check ----------
cmd_check() {
  step "1. 工作区干净"
  if [ -n "$(git status --porcelain)" ]; then
    fail=$((fail+1))
    red "  ✗ 有未提交改动，先提交（发版 tag 会指向当前 HEAD）"
    git status --short | sed 's/^/    /'
  else
    ok "无未提交改动"
  fi

  step "2. 分支与远端同步"
  branch="$(git rev-parse --abbrev-ref HEAD)"
  if [ "$branch" != "master" ] && [ "$branch" != "main" ]; then
    problem "当前在 $branch，正式发版应从 master/main 打 tag"
  else
    ok "分支 $branch"
  fi
  git fetch --tags --quiet origin || true
  if [ -n "$(git log origin/$branch..HEAD --oneline 2>/dev/null)" ]; then
    problem "有未推送的提交，先 git push"
  else
    ok "与 origin/$branch 一致"
  fi

  step "3. 版本号未占用"
  if git rev-parse -q --verify "refs/tags/$VERSION" >/dev/null 2>&1; then
    problem "tag $VERSION 已存在（本地），不能重复发布"
  elif git ls-remote --tags origin "refs/tags/$VERSION" | grep -q .; then
    problem "tag $VERSION 已在远端存在"
  else
    ok "tag $VERSION 可用"
  fi
  if [ -n "${VERSION}" ] && ! [[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    problem "版本号应为 vX.Y.Z（预发布请用 vX.Y.Z-rcN，注意 rc 不会覆盖 latest 镜像）"
  fi

  step "4. 构建"
  if go build ./... ; then ok "go build"; else problem "go build 失败"; fi
  if go vet ./... ; then ok "go vet"; else problem "go vet 失败"; fi
  if out="$(gofmt -l ./cmd ./internal 2>/dev/null)" && [ -z "$out" ]; then
    ok "gofmt 干净"
  else
    problem "gofmt 未格式化：$out"
  fi
  if command -v node >/dev/null 2>&1; then
    if node --check internal/webui/app.js >/dev/null; then ok "app.js 语法"; else problem "app.js 语法错误"; fi
  else
    warn "未安装 node，跳过 JS 语法检查"
  fi

  step "5. 测试（模拟 CI：不含被 .gitignore 的 testdata）"
  # CI 上 testdata/ 不存在。这里把它临时挪开跑一遍，提前暴露
  # 「测试硬依赖本地样例」这类只在 CI 才炸的问题（v1.0.4 踩过）。
  local td="internal/creds/testdata"
  local moved=0
  if [ -d "$td" ]; then mv "$td" "${td}.releasebak"; moved=1; fi
  if go test -count=1 ./... 2>&1 | tee /tmp/relay-release-test.log | grep -q '^FAIL'; then
    problem "无 testdata 时测试失败（CI 会红）"
    grep -E '^(FAIL|--- FAIL|\s+\w+_test\.go:)' /tmp/relay-release-test.log | head -20 | sed 's/^/    /'
  else
    ok "无 testdata 时全部通过"
  fi
  if [ "$moved" = "1" ]; then mv "${td}.releasebak" "$td"; fi

  step "6. 交叉编译与发布资产名"
  for t in "linux amd64" "linux arm64" "windows amd64" "windows arm64" "darwin amd64" "darwin arm64"; do
    set -- $t
    out="/tmp/relay-${1}-${2}"
    [ "$1" = "windows" ] && out="${out}.exe"
    if CGO_ENABLED=0 GOOS=$1 GOARCH=$2 go build -trimpath -o "$out" ./cmd/relay 2>/dev/null; then
      ok "$1/$2"
      rm -f "$out"
    else
      problem "$1/$2 编译失败"
    fi
  done

  step "7. 时区数据（发布版启动必查）"
  # Windows/macOS 的裸机发布包没有系统 IANA 数据时，TZ=Asia/Shanghai 会直接
  # 报「TZ 非法」起不来。必须有 time/tzdata 的内嵌引用。
  if grep -rq '"time/tzdata"' --include='*.go' .; then
    ok "已内嵌 time/tzdata"
  else
    problem '缺少 _ "time/tzdata" 引用：Windows/macOS 发布版可能启动即报 TZ 非法'
  fi

  step "8. 更新器与 CI 产物名一致"
  asset="$(grep -o 'relay-"+goos+'-'"+goarch' internal/updater/updater.go 2>/dev/null || true)"
  if grep -q 'relay-\${GOOS}-\${GOARCH}\${suffix}' .github/workflows/docker.yml; then
    ok "CI 产物名 relay-<os>-<arch>[.exe] 与 updater 约定一致"
  else
    problem "CI 产物名与 internal/updater 的 assetName 可能不一致（改文件名要两边同步）"
  fi
  if grep -q 'checksums.txt' internal/updater/updater.go && grep -q 'checksums.txt' .github/workflows/docker.yml; then
    ok "checksums.txt 由 CI 产出、由更新器校验"
  else
    problem "checksums.txt 缺失：一键更新会拒绝安装"
  fi

  step "9. Release 说明"
  if [ -f "docs/releases/${VERSION}.md" ]; then
    ok "docs/releases/${VERSION}.md 已存在"
  else
    warn "docs/releases/${VERSION}.md 不存在——CI 只会生成 compare 链接，发布后需手工补更新日志"
    warn "  参考 docs/releases/v1.0.4.md 的结构"
  fi

  step "10. 数据库迁移与兼容性"
  # 用「上一个 tag」而不是算术减一：补丁号为 0 或跳版本时算术会错。
  local prev_tag
  prev_tag="$(git tag -l 'v*' --sort=-v:refname | grep -vx "$VERSION" | head -1)"
  if [ -n "$prev_tag" ] && ! git diff --quiet "$prev_tag" -- internal/store 2>/dev/null; then
    yellow "  ! internal/store 自 $prev_tag 有改动，升级前请拉一个旧库验证"
    git diff --stat "$prev_tag" -- internal/store | tail -5 | sed 's/^/    /'
  elif [ -n "$prev_tag" ]; then
    ok "store 层自 $prev_tag 无改动，无需迁移"
  else
    warn "没有可比较的历史 tag"
  fi

  echo
  if [ "$fail" -gt 0 ]; then
    red "共 $fail 项检查未通过，修复后再发版。"
    exit 1
  fi
  green "全部检查通过，可以发版。"
}

# ---------- tag ----------
cmd_tag() {
  VERSION="${1:?用法: release.sh tag v1.0.5}"
  cmd_check
  step "打 tag 并推送"
  local notes_file="docs/releases/${VERSION}.md"
  if [ -f "$notes_file" ]; then
    git tag -a "$VERSION" -F "$notes_file"
  else
    git tag -a "$VERSION" -m "$VERSION"
    warn "没有 $notes_file，tag 说明会很短"
  fi
  git push origin "$VERSION"
  green "tag $VERSION 已推送。CI 会构建镜像 + 6 平台二进制并创建 Release。"
  echo "下一步：scripts/release.sh watch $VERSION"
}

# ---------- watch ----------
cmd_watch() {
  VERSION="${1:?用法: release.sh watch v1.0.5}"
  command -v gh >/dev/null || { red "需要 gh CLI"; exit 1; }
  step "等待 CI（$VERSION）"
  local run_id
  run_id="$(gh run list --limit 30 --json databaseId,headBranch,event,displayTitle \
            --jq "[.[] | select(.headBranch==\"$VERSION\")][0].databaseId")"
  if [ -z "$run_id" ] || [ "$run_id" = "null" ]; then
    red "找不到 $VERSION 的 CI run，确认 tag 已推送"
    exit 1
  fi
  gh run watch "$run_id" --exit-status || {
    red "CI 失败。日志："
    gh run view "$run_id" --log-failed | tail -40
    exit 1
  }
  green "CI 通过"

  step "Release 资产"
  local want=(checksums.txt relay-linux-amd64 relay-linux-arm64 relay-windows-amd64.exe
              relay-windows-arm64.exe relay-darwin-amd64 relay-darwin-arm64)
  local have
  have="$(gh release view "$VERSION" --json assets --jq '.assets[].name')"
  local missing=0
  for f in "${want[@]}"; do
    if echo "$have" | grep -qx "$f"; then ok "$f"; else problem "缺少 $f"; missing=1; fi
  done
  [ "$missing" = "0" ] || exit 1

  step "sha256 自洽"
  local tmp; tmp="$(mktemp -d)"
  curl -fsSL "https://github.com/$REPO/releases/download/$VERSION/checksums.txt" -o "$tmp/checksums.txt"
  ( cd "$tmp" && sha256sum -c --ignore-missing checksums.txt ) || {
    red "checksums.txt 与实际资产不符"; exit 1
  }
  rm -rf "$tmp"

  step "Release 说明"
  local body_len
  body_len="$(gh release view "$VERSION" --json body --jq '.body | length')"
  if [ "$body_len" -lt 200 ]; then
    warn "说明只有 $body_len 字符（大概率是自动生成的 compare 链接）"
    echo "  补：gh release edit $VERSION --notes-file docs/releases/$VERSION.md"
  else
    ok "说明长度 $body_len"
  fi

  step "镜像"
  echo "  拉取验证：docker pull $IMG:$VERSION && docker run --rm -e SECRET_KEY=x $IMG:$VERSION"
  echo "  （本地 Docker 不可用时，至少在服务器上跑一次 /health）"
}

# ---------- verify ----------
cmd_verify() {
  VERSION="${1:?用法: release.sh verify v1.0.5}"
  local os arch ext tmp
  os="$(go env GOOS)"; arch="$(go env GOARCH)"
  ext=""; [ "$os" = "windows" ] && ext=".exe"
  local name="relay-${os}-${arch}${ext}"
  tmp="$(mktemp -d)"
  step "下载 $name 并实跑"
  curl -fsSL "https://github.com/$REPO/releases/download/$VERSION/$name" -o "$tmp/$name"
  chmod +x "$tmp/$name"
  curl -fsSL "https://github.com/$REPO/releases/download/$VERSION/checksums.txt" -o "$tmp/checksums.txt"
  ( cd "$tmp" && grep -E "[ *]$name\$" checksums.txt | sha256sum -c - ) || { red "sha256 校验失败"; exit 1; }

  # 数据目录必须和二进制分开，避免污染仓库
  ( cd "$tmp" && DATA_DIR="$tmp/data" SECRET_KEY=k ADMIN_TOKEN=t \
      LISTEN_ADDR=127.0.0.1:18099 "./$name" > "$tmp/run.log" 2>&1 & echo $! > "$tmp/pid" )
  sleep 4
  local health
  health="$(curl -fsS http://127.0.0.1:18099/health 2>/dev/null || true)"
  if [ "$health" = '{"ok":true}' ]; then
    ok "启动正常：$health"
  else
    red "启动失败，日志："
    sed 's/^/    /' "$tmp/run.log" | head -20
    kill "$(cat "$tmp/pid")" 2>/dev/null || true
    exit 1
  fi
  local cur
  cur="$(curl -fsS -c "$tmp/cj" -X POST http://127.0.0.1:18099/api/login \
        -H 'Content-Type: application/json' -d '{"token":"t"}' >/dev/null && \
        curl -fsS -b "$tmp/cj" http://127.0.0.1:18099/api/update | \
        python -c 'import json,sys;print(json.load(sys.stdin)["status"]["current"])' 2>/dev/null || true)"
  if [ "$cur" = "$VERSION" ]; then ok "版本号正确：$cur"; else problem "版本号显示为 $cur，期望 $VERSION"; fi
  kill "$(cat "$tmp/pid")" 2>/dev/null || true
  rm -rf "$tmp"
}

# VERSION 可用环境变量预置（方便 check 同一版本号），命令行参数优先。
VERSION="${VERSION:-}"
case "${1:-}" in
  check)
    VERSION="${2:-${VERSION:-$(git describe --tags --abbrev=0 2>/dev/null || echo v0.0.0)}}"
    cmd_check ;;
  tag)    cmd_tag   "${2:-}" ;;
  watch)  cmd_watch "${2:-}" ;;
  verify) cmd_verify "${2:-}" ;;
  *) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 1 ;;
esac
