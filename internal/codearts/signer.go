package codearts

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// 华为云 SDK-HMAC-SHA256 请求签名。
//
// 这套签名的坑集中在三处，任何一处不对都会得到 APIG.0301：
//
//  1. canonical URI 永远以 "/" 结尾（上游按带斜杠的路径校验）；
//  2. canonical query 按「键、值」两级排序，并用 RFC3986 编码（含 !'()*）；
//  3. 参与签名的头是**全部小写**的头名排序后逐行 "k:v"，空行后接
//     ";".join(headers)，再接 body 的 sha256。
//
// 与 Go 自己的语义对齐：path 使用「已解码再按 Go 规则转义」的形式，
// query 用 url.ParseQuery 的结果（"+" 视作空格）。这两点在插件里也是刻意做的。

const (
	sdkAlgo     = "SDK-HMAC-SHA256"
	emptySha256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	dateLayout  = "20060102T150405Z"
	authPrefix  = "SDK-HMAC-SHA256 "
)

// SignRequest 给一次请求签名，返回需要真的发出去的头（含 Authorization）。
//
// headers 里应包含所有要与签名一起发出的头；函数会补 host / x-sdk-date /
// x-sdk-content-sha256 / x-security-token 与（非 GET 的）content-type。
// 返回的头里**不含** host：那是传输层自己设置的，显式设置反而可能重复。
func SignRequest(ak, sk, securityToken, method, rawURL string, body []byte, extra map[string]string, at time.Time) (map[string]string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("解析签名地址失败: %w", err)
	}
	// canonical URI：拿路径原文（url.Path 已解码），保证末尾有 "/"。
	// 再按 RFC3986 转义：Go 的 url.Path 是解码后的原文，上游签的是编码后的路径。
	uri := rfc3986Path(u.Path)
	if uri == "" {
		uri = "/"
	}
	if !strings.HasSuffix(uri, "/") {
		uri += "/"
	}
	query := canonicalQuery(u.RawQuery)
	date := at.UTC().Format(dateLayout)
	payloadHash := sha256Hex(body)

	h := map[string]string{
		"host":                 u.Host,
		"x-sdk-date":           date,
		"x-sdk-content-sha256": payloadHash,
		"x-security-token":     securityToken,
	}
	for k, v := range extra {
		h[strings.ToLower(k)] = v
	}
	if !strings.EqualFold(method, http.MethodGet) {
		if _, ok := h["content-type"]; !ok {
			h["content-type"] = "application/json"
		}
	}

	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	var lines []string
	for _, k := range names {
		lines = append(lines, k+":"+h[k])
	}
	signedHeaders := strings.Join(names, ";")
	canonical := strings.Join([]string{
		strings.ToUpper(method),
		uri,
		query,
		strings.Join(lines, "\n"),
		"",
		signedHeaders,
		payloadHash,
	}, "\n")

	stringToSign := sdkAlgo + "\n" + date + "\n" + sha256Hex([]byte(canonical))
	mac := hmac.New(sha256.New, []byte(sk))
	mac.Write([]byte(stringToSign))
	sig := hex.EncodeToString(mac.Sum(nil))

	out := make(map[string]string, len(h))
	for k, v := range h {
		if k == "host" {
			continue
		}
		out[k] = v
	}
	out["Authorization"] = authPrefix + "Access=" + ak + ",SignedHeaders=" + signedHeaders + ",Signature=" + sig
	return out, nil
}

// canonicalQuery 按「键、值」两级排序并用 RFC3986 编码。
func canonicalQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	vals, err := url.ParseQuery(rawQuery)
	if err != nil {
		// ParseQuery 遇到坏片段会部分失败；退回按原始串手工拆。
		vals = url.Values{}
		for _, pair := range strings.Split(rawQuery, "&") {
			if pair == "" {
				continue
			}
			kv := strings.SplitN(pair, "=", 2)
			k, _ := url.QueryUnescape(kv[0])
			v := ""
			if len(kv) == 2 {
				v, _ = url.QueryUnescape(kv[1])
			}
			vals.Add(k, v)
		}
	}
	type kv struct{ k, v string }
	var pairs []kv
	for k, vs := range vals {
		for _, v := range vs {
			pairs = append(pairs, kv{k, v})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, rfc3986(p.k)+"="+rfc3986(p.v))
	}
	return strings.Join(parts, "&")
}

// rfc3986 是 encodeURIComponent 的 Go 等价物，额外把 !'()* 转义。
func rfc3986(s string) string {
	var b strings.Builder
	for _, by := range []byte(s) {
		if isUnreserved(by) {
			b.WriteByte(by)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", by)
	}
	return b.String()
}

// rfc3986Path 按路径段编码：保留 "/"，其余非 unreserved 字节转义。
func rfc3986Path(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = rfc3986(s)
	}
	return strings.Join(segs, "/")
}

func isUnreserved(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '-', '_', '.', '~':
		return true
	}
	return false
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
