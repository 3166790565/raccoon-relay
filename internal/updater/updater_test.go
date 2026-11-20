package updater

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.0.3", "v1.0.2", 1},
		{"v1.0.2", "v1.0.3", -1},
		{"1.0.3", "v1.0.3", 0},
		{"v1.0", "v1.0.0", 0},
		{"v1.10.0", "v1.9.9", 1},
		{"2026.11.13", "2026.11.12", 1},
		{"2026.11.13", "v1.0.3", 1},
		{"v1.1.0-rc1", "v1.1.0", 0}, // 预发布后缀不参与比较
		{"", "v1.0.0", -1},
		{"v2", "v10", -1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q,%q)=%d，期望 %d", c.a, c.b, got, c.want)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	const raw = `# comment
0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef  relay-linux-amd64
ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef0123456789 *relay-windows-amd64.exe
badline
`
	m, err := parseChecksums(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 {
		t.Fatalf("应解析出 2 条，实际 %d", len(m))
	}
	if got := m["relay-linux-amd64"]; got != "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Fatalf("linux 校验值错误: %s", got)
	}
	// 二进制模式前缀 * 与大小写都要归一
	if got := m["relay-windows-amd64.exe"]; got != "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789" {
		t.Fatalf("windows 校验值错误: %s", got)
	}
}

func TestAssetName(t *testing.T) {
	if got := assetName("linux", "amd64"); got != "relay-linux-amd64" {
		t.Fatalf("linux 资产名错误: %s", got)
	}
	if got := assetName("windows", "arm64"); got != "relay-windows-arm64.exe" {
		t.Fatalf("windows 资产名错误: %s", got)
	}
}

// CI 在非 tag 构建里注入的是 commit SHA：不能拿它做版本比较，
// 否则 SHA 以字母开头时 "v1.x" 恒大于它，面板会永远显示「有更新」。
func TestIsUnversioned(t *testing.T) {
	for _, v := range []string{"", "dev", "DEV", "a1b2c3d4e5f6", "deadbeefdeadbeef", "a1b2c3d4e5f6a7b8"} {
		if !isUnversioned(v) {
			t.Fatalf("%q 应视为无法比较的版本", v)
		}
	}
	for _, v := range []string{"v1.0.3", "1.2", "2026.11.13", "v1.0.4-rc1"} {
		if isUnversioned(v) {
			t.Fatalf("%q 是正式版本号，不该被当成未知", v)
		}
	}
}

// 校验文件不能走下载镜像，且 token 只能发回 GitHub API 主机。
func TestChecksumAndTokenHostPolicy(t *testing.T) {
	if sameHost("https://gh-proxy.com/https://github.com/x/y", "https://api.github.com") {
		t.Fatal("镜像域名不该被当作 GitHub API 主机")
	}
	if !sameHost("https://api.github.com/repos/a/b/releases/assets/1", "https://api.github.com") {
		t.Fatal("同主机应判定为 GitHub API")
	}
}

func TestParseMirrors(t *testing.T) {
	got := ParseMirrors(" https://gh-proxy.com/ ,cdn.gh-proxy.org\nhttps://o-o.men ")
	want := []string{"https://gh-proxy.com", "https://cdn.gh-proxy.org", "https://o-o.men"}
	if len(got) != len(want) {
		t.Fatalf("镜像数量错误: %#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个镜像错误: %q，期望 %q", i, got[i], want[i])
		}
	}
	if ParseMirrors("") != nil {
		t.Fatal("空字符串应返回 nil，交给默认镜像")
	}
}

// 默认启用三个镜像；显式设置 UPDATE_DOWNLOAD_BASE 时只走那一个，不再兜底。
func TestMirrorDefaultsAndOverride(t *testing.T) {
	u := New(Options{Current: "v1.0.0"})
	if len(u.opts.Mirrors) != 3 {
		t.Fatalf("默认应有 3 个镜像，实际 %#v", u.opts.Mirrors)
	}
	u = New(Options{Current: "v1.0.0", DownloadBase: "https://ghfast.top"})
	if u.opts.Mirrors != nil {
		t.Fatalf("指定 DownloadBase 后不该再有镜像列表: %#v", u.opts.Mirrors)
	}
	srcs := u.downloadSources("https://github.com/x/y/releases/download/v1/a")
	if len(srcs) != 1 || srcs[0] != "https://ghfast.top/https://github.com/x/y/releases/download/v1/a" {
		t.Fatalf("DownloadBase 应只走该镜像: %#v", srcs)
	}
}

// apiEndpoints 只对官方 API 追加镜像；自定义 API 原样使用。
func TestAPIEndpoints(t *testing.T) {
	u := New(Options{Current: "v1.0.0", Repo: "o/r"})
	eps := u.apiEndpoints()
	if len(eps) != 4 {
		t.Fatalf("应有直连 + 3 个镜像，实际 %d: %#v", len(eps), eps)
	}
	if eps[0] != "https://api.github.com/repos/o/r/releases/latest" {
		t.Fatalf("直连端点错误: %s", eps[0])
	}
	if eps[1] != "https://gh-proxy.com/https://api.github.com/repos/o/r/releases/latest" {
		t.Fatalf("镜像端点错误: %s", eps[1])
	}
	u = New(Options{Current: "v1.0.0", Repo: "o/r", APIBase: "https://mirror.example/api"})
	if eps := u.apiEndpoints(); len(eps) != 1 {
		t.Fatalf("自定义 API 不应追加镜像: %#v", eps)
	}
}

// 直连失败时自动换镜像下载，并记录实际使用的来源。
func TestDownloadFallsBackToMirror(t *testing.T) {
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer direct.Close()
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer mirror.Close()

	u := New(Options{
		Current:    "v1.0.0",
		APIBase:    "https://api.github.com",
		Mirrors:    []string{mirror.URL},
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	resp, err := u.downloadAsset(context.Background(), direct.URL+"/asset")
	if err != nil {
		t.Fatalf("应回退到镜像: %v", err)
	}
	resp.Body.Close()
	u.mu.Lock()
	src := u.lastSrc
	u.mu.Unlock()
	if src != strings.TrimPrefix(mirror.URL, "http://") {
		t.Fatalf("应记录镜像来源，实际 %q", src)
	}
}

// 直连与所有镜像都失败时返回错误，且错误信息里带首个失败源。
func TestDownloadAllSourcesFail(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer bad.Close()

	u := New(Options{
		Current:    "v1.0.0",
		Mirrors:    []string{bad.URL},
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	if _, err := u.downloadAsset(context.Background(), bad.URL+"/asset"); err == nil {
		t.Fatal("全部失败时应返回错误")
	}
}
