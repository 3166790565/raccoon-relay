package updater

import (
	"strings"
	"testing"
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
	if sameHost("https://ghfast.top/https://github.com/x/y", "https://api.github.com") {
		t.Fatal("镜像域名不该被当作 GitHub API 主机")
	}
	if !sameHost("https://api.github.com/repos/a/b/releases/assets/1", "https://api.github.com") {
		t.Fatal("同主机应判定为 GitHub API")
	}
}
