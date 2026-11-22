package autoclaw

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"runtime"
	"strings"
	"time"
)

// 一组不依赖具体业务的小工具。渠道包里各自带一份，避免为了几个字符串函数
// 在公共包里堆一堆只有两家用得到的 API。

func strOf(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func numOf(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		f, _ := t.Float64()
		return f
	}
	return 0
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func shorten(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func strPtr(s string) *string { return &s }
func int64Ptr(n int64) *int64 { return &n }

func nowUnix() int64 { return time.Now().Unix() }

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// jsonValue 把 RawMessage 解成 any；空值返回 nil。
func jsonValue(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}

// randomHex 生成 n 字节随机数的十六进制串（2n 个字符）。
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// b64urlDecode 解 base64url（容忍缺失的 padding）。
func b64urlDecode(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	return base64.RawURLEncoding.DecodeString(s)
}

// platformTm 复刻官方客户端的「平台」写法：darwin → mac，windows → win。
func platformTm() string {
	switch runtime.GOOS {
	case "darwin":
		return "mac"
	case "windows":
		return "win"
	}
	return "linux"
}

// archName 是 UA 里用的架构名（x64 / arm64 / …）。
func archName() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	}
	return runtime.GOARCH
}
