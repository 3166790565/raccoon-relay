// Package updater 实现「从 GitHub Release 检查并安装新版本」。
//
// 设计边界：
//   - 只信任 HTTPS + sha256 校验；release 里没有 checksums.txt 就拒绝安装。
//   - 替换目标是当前正在运行的二进制（容器内是 /app/relay，裸跑是 os.Executable()）。
//   - 安装完成后由调用方决定如何重启：容器退出交给 restart 策略，Linux/macOS 直接 exec 自身。
package updater

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	defaultRepo    = "3166790565/raccoon-relay"
	defaultAPIBase = "https://api.github.com"
	defaultTTL     = 10 * time.Minute
	maxAssetBytes  = 200 << 20 // 200MB，远超单二进制体积，防止异常响应打满磁盘
)

// Release 是 GitHub Release 里与自更新相关的字段。
type Release struct {
	Version     string    `json:"version"`
	Notes       string    `json:"notes"`
	PageURL     string    `json:"pageUrl"`
	PublishedAt time.Time `json:"publishedAt"`
	AssetName   string    `json:"assetName"`
	AssetURL    string    `json:"assetUrl"`
	ChecksumURL string    `json:"checksumUrl"`
	AssetSize   int64     `json:"assetSize"`
}

// Status 是面板直接展示的状态。
type Status struct {
	Current         string `json:"current"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"updateAvailable"`
	Notes           string `json:"notes"`
	PageURL         string `json:"pageUrl"`
	PublishedAt     string `json:"publishedAt"`
	CheckedAt       int64  `json:"checkedAt"` // 毫秒，前端 fmtTime 直接可用
	Error           string `json:"error,omitempty"`
	CanSelfUpdate   bool   `json:"canSelfUpdate"`
	AutoRestart     bool   `json:"autoRestart"`
	Mode            string `json:"mode"` // docker | binary
	Asset           string `json:"asset"`
	BackupAvailable bool   `json:"backupAvailable"`
	InstalledAt     int64  `json:"installedAt,omitempty"`
}

// Options 由 main 从环境变量装配。
type Options struct {
	Repo         string
	Current      string
	Token        string // 可选 GitHub Token，提高 API 限额
	APIBase      string
	DownloadBase string // 可选镜像前缀，如 https://ghfast.top/
	TargetPath   string // 可选覆盖安装目标
	DataDir      string // 容器内安装到数据卷，重启/重建容器都不丢
	HTTPClient   *http.Client
	TTL          time.Duration
}

type Updater struct {
	opts   Options
	client *http.Client

	mu        sync.Mutex
	lastRel   *Release
	lastErr   string
	checkedAt time.Time
}

func New(opts Options) *Updater {
	if opts.Repo == "" {
		opts.Repo = defaultRepo
	}
	if opts.APIBase == "" {
		opts.APIBase = defaultAPIBase
	}
	if opts.TTL <= 0 {
		opts.TTL = defaultTTL
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	return &Updater{opts: opts, client: client}
}

// InContainer 判断是否运行在 Docker 容器里。Dockerfile 会显式设置 RACCOON_DOCKER=1，
// /.dockerenv 作为兼容旧镜像的兜底。
func InContainer() bool {
	if os.Getenv("RACCOON_DOCKER") == "1" {
		return true
	}
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

// RestartSupported 表示安装后能否自动重启。
// 容器靠 restart 策略；Unix 裸跑靠 exec；Windows 裸跑只能提示手动重启。
func RestartSupported() bool {
	return InContainer() || runtime.GOOS != "windows"
}

// Refresh 拉取最新 release。force=false 时走 TTL 缓存，避免面板每次刷新都打 GitHub。
func (u *Updater) Refresh(ctx context.Context, force bool) Status {
	u.mu.Lock()
	defer u.mu.Unlock()

	if !force && !u.checkedAt.IsZero() && time.Since(u.checkedAt) < u.opts.TTL {
		return u.statusLocked()
	}

	rel, err := u.fetchLatest(ctx)
	u.checkedAt = time.Now()
	if err != nil {
		u.lastErr = err.Error()
	} else {
		u.lastErr = ""
		u.lastRel = rel
	}
	return u.statusLocked()
}

// LatestRelease 返回最近一次成功检查到的 release 快照，供 Install 使用。
func (u *Updater) LatestRelease() *Release {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.lastRel == nil {
		return nil
	}
	cp := *u.lastRel
	return &cp
}

func (u *Updater) statusLocked() Status {
	st := Status{
		Current:       u.opts.Current,
		Mode:          "binary",
		CanSelfUpdate: u.probeWritable(),
		AutoRestart:   RestartSupported(),
		Asset:         assetName(runtime.GOOS, runtime.GOARCH),
	}
	if InContainer() {
		st.Mode = "docker"
	}
	if info, err := os.Stat(u.backupPath()); err == nil {
		st.BackupAvailable = !info.IsDir()
		st.InstalledAt = info.ModTime().UnixMilli()
	}
	if !u.checkedAt.IsZero() {
		st.CheckedAt = u.checkedAt.UnixMilli()
	}
	if u.lastRel != nil {
		st.Latest = u.lastRel.Version
		st.Notes = u.lastRel.Notes
		st.PageURL = u.lastRel.PageURL
		if !u.lastRel.PublishedAt.IsZero() {
			st.PublishedAt = u.lastRel.PublishedAt.Format(time.RFC3339)
		}
	} else if !u.checkedAt.IsZero() && u.lastErr == "" {
		// 请求成功但没拿到可用 release（仓库还没有 Release，或最新版没有本平台资产）。
		// 这不是网络/限流错误，面板显示「暂无可用版本」比「检查失败」准确。
		st.Latest = ""
		st.UpdateAvailable = false
	}
	if u.lastRel != nil {
		if isUnversioned(u.opts.Current) {
			st.UpdateAvailable = st.Latest != ""
		} else {
			st.UpdateAvailable = Compare(st.Latest, u.opts.Current) > 0
		}
	}
	st.Error = u.lastErr
	return st
}

// isUnversioned 判断当前版本号是否无法参与数值比较：
// 空串、dev，以及只由十六进制字符组成的 commit SHA（CI 在非 tag 构建里注入）。
func isUnversioned(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || strings.EqualFold(v, "dev") {
		return true
	}
	if _, ok := parseVersion(v); ok {
		return false
	}
	for _, c := range strings.ToLower(v) {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return len(v) >= 7
}

// ---------- GitHub API ----------

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func (u *Updater) fetchLatest(ctx context.Context) (*Release, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	endpoint := strings.TrimRight(u.opts.APIBase, "/") + "/repos/" + u.opts.Repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "raccoon-relay/"+u.opts.Current)
	if u.opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+u.opts.Token)
	}

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 GitHub 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// 私有仓库未授权时 GitHub 也回 404，避免暴露仓库是否存在。
		return nil, fmt.Errorf("仓库 %s 不存在、是私有仓库，或还没有发布 Release（请检查 UPDATE_REPO / GITHUB_TOKEN）", u.opts.Repo)
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		// 403 不只是限流：token 缺权限、SSO 未授权也会回 403，按响应头区分。
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return nil, errors.New("GitHub API 限流，请稍后再试（可设置 GITHUB_TOKEN 提高限额）")
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("GitHub API 拒绝访问（%d）：%s。请检查 GITHUB_TOKEN 权限",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("GitHub API 返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("解析 Release 失败: %w", err)
	}
	if rel.Draft || rel.Prerelease {
		// releases/latest 正常不会返回草稿/预发布；这里只是防御。
		return nil, errors.New("最新发布是草稿或预发布版本")
	}

	out := &Release{
		Version:     rel.TagName,
		Notes:       strings.TrimSpace(rel.Body),
		PageURL:     rel.HTMLURL,
		PublishedAt: rel.PublishedAt,
	}
	want := assetName(runtime.GOOS, runtime.GOARCH)
	for _, a := range rel.Assets {
		switch a.Name {
		case want:
			out.AssetName = a.Name
			out.AssetURL = a.BrowserDownloadURL
			out.AssetSize = a.Size
		case "checksums.txt":
			out.ChecksumURL = a.BrowserDownloadURL
		}
	}
	if out.AssetName == "" {
		return nil, fmt.Errorf("版本 %s 没有提供 %s 安装包", rel.TagName, want)
	}
	if out.ChecksumURL == "" {
		return nil, fmt.Errorf("版本 %s 缺少 checksums.txt，拒绝无校验更新", rel.TagName)
	}
	return out, nil
}

// ---------- 安装 ----------

// Install 下载当前平台对应的二进制，校验 sha256 后原子替换目标文件。
// 返回替换后的路径。
func (u *Updater) Install(ctx context.Context, rel *Release) (string, error) {
	if rel == nil {
		return "", errors.New("还没有检查到可用版本")
	}
	target, err := u.targetPath()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建安装目录失败: %w", err)
	}

	sums, err := u.fetchChecksums(ctx, rel.ChecksumURL)
	if err != nil {
		return "", err
	}
	want, ok := sums[rel.AssetName]
	if !ok {
		return "", fmt.Errorf("checksums.txt 中没有 %s", rel.AssetName)
	}

	resp, err := u.get(ctx, u.assetURL(rel.AssetURL))
	if err != nil {
		return "", fmt.Errorf("下载安装包失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载安装包返回 %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp(dir, ".relay-update-*")
	if err != nil {
		return "", fmt.Errorf("安装目录不可写: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxAssetBytes+1))
	if err != nil {
		return "", fmt.Errorf("写入安装包失败: %w", err)
	}
	if n > maxAssetBytes {
		return "", errors.New("安装包超过大小上限")
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return "", fmt.Errorf("sha256 校验失败（期望 %s，实际 %s）", want, got)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil && runtime.GOOS != "windows" {
		return "", fmt.Errorf("设置可执行权限失败: %w", err)
	}

	// 先留一份旧版本，安装失败或新版本起不来时能回滚。
	// 首次更新时目标还不存在（例如容器里 /data/bin/relay 尚无文件），此时没有旧版本可备份。
	backup := u.backupPath()
	if _, statErr := os.Stat(target); statErr == nil {
		if err := copyFile(target, backup); err != nil {
			return "", fmt.Errorf("备份当前版本失败: %w", err)
		}
	}
	if err := replaceBinary(tmpName, target); err != nil {
		return "", fmt.Errorf("替换二进制失败: %w", err)
	}
	cleanup = false
	return target, nil
}

// Rollback 把最近一次备份写回安装目标。
func (u *Updater) Rollback() (string, error) {
	target, err := u.targetPath()
	if err != nil {
		return "", err
	}
	backup := u.backupPath()
	if info, err := os.Stat(backup); err != nil || info.IsDir() {
		return "", errors.New("没有可回滚的备份")
	}
	if err := copyFile(backup, target); err != nil {
		return "", fmt.Errorf("回滚失败: %w", err)
	}
	return target, nil
}

// backupPath 与安装目标同目录，保证 rename/copy 发生在同一文件系统上。
func (u *Updater) backupPath() string {
	target, err := u.targetPath()
	if err != nil {
		return ""
	}
	return target + ".bak"
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	mode := os.FileMode(0o755)
	if info, err := in.Stat(); err == nil {
		mode = info.Mode().Perm()
	}
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil && runtime.GOOS != "windows" {
		_ = os.Remove(tmp)
		return err
	}
	return replaceBinary(tmp, dst)
}

func (u *Updater) fetchChecksums(ctx context.Context, raw string) (map[string]string, error) {
	// 校验文件必须从原站取（不走镜像）：镜像若能同时替换二进制和校验值，
	// sha256 校验就形同虚设。
	resp, err := u.get(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("下载校验文件失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载校验文件返回 %d", resp.StatusCode)
	}
	return parseChecksums(io.LimitReader(resp.Body, 4<<20))
}

func (u *Updater) get(ctx context.Context, raw string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "raccoon-relay/"+u.opts.Current)
	// 只把 GITHUB_TOKEN 发回 GitHub API 主机的 release 资产：
	// 走镜像（UPDATE_DOWNLOAD_BASE）时绝不附带，否则 token 就泄露给第三方了。
	if u.opts.Token != "" && sameHost(raw, u.opts.APIBase) {
		req.Header.Set("Authorization", "Bearer "+u.opts.Token)
	}
	return u.client.Do(req)
}

// sameHost 判断 raw 是否与 base 同主机（忽略 scheme 与端口差异带来的大小写/末尾斜杠问题）。
func sameHost(raw, base string) bool {
	ru, err := url.Parse(raw)
	if err != nil {
		return false
	}
	bu, err := url.Parse(base)
	if err != nil {
		return false
	}
	return strings.EqualFold(ru.Host, bu.Host)
}

// assetURL 支持只前缀镜像（例如 https://ghfast.top/），不改写 GitHub API 返回的路径。
func (u *Updater) assetURL(raw string) string {
	if raw == "" {
		return raw
	}
	base := strings.TrimRight(u.opts.DownloadBase, "/")
	if base == "" {
		return raw
	}
	if _, err := url.Parse(raw); err != nil {
		return raw
	}
	return base + "/" + raw
}

func (u *Updater) targetPath() (string, error) {
	if u.opts.TargetPath != "" {
		return u.opts.TargetPath, nil
	}
	if InContainer() {
		// 容器内写到持久化数据卷：重启、重建容器都还在。
		// 注意重新 docker pull 新镜像不会覆盖它，需要删掉 /data/bin/relay 才会回到镜像版本。
		if u.opts.DataDir != "" {
			return filepath.Join(u.opts.DataDir, "bin", "relay"), nil
		}
		return "/app/relay", nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("无法定位当前程序: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

func (u *Updater) probeWritable() bool {
	target, err := u.targetPath()
	if err != nil {
		return false
	}
	dir := filepath.Dir(target)
	// 容器里 /data/bin 是安装时才创建的，探测时还不存在。先建目录再探，
	// 否则全新部署会一直报「目录不可写」而把按钮藏起来。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".rc-update-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// replaceBinary 用同目录临时文件替换目标。Windows 不能覆盖正在运行的 exe，
// 先把旧文件改名再落新文件；失败则回滚。
func replaceBinary(tmp, target string) error {
	if runtime.GOOS == "windows" {
		// 目标不存在（首次安装）时直接落位，不能先 rename 旧文件。
		if _, err := os.Stat(target); err != nil {
			return os.Rename(tmp, target)
		}
		old := target + ".old"
		_ = os.Remove(old)
		if err := os.Rename(target, old); err != nil {
			return err
		}
		if err := os.Rename(tmp, target); err != nil {
			_ = os.Rename(old, target)
			return err
		}
		_ = os.Remove(old)
		return nil
	}
	return os.Rename(tmp, target)
}

// ---------- 工具 ----------

func assetName(goos, goarch string) string {
	name := "relay-" + goos + "-" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

func parseChecksums(r io.Reader) (map[string]string, error) {
	out := make(map[string]string)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sum := strings.ToLower(fields[0])
		if len(sum) != 64 || !isHex(sum) {
			continue
		}
		name := filepath.Base(strings.TrimPrefix(fields[1], "*"))
		out[name] = sum
	}
	return out, sc.Err()
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
