package updater

import (
	"strconv"
	"strings"
)

// Compare 比较两个版本号，返回 -1 / 0 / 1。
//
// 支持的写法：
//   - v1.0.3、1.0.3、v1.2（缺段按 0 补）
//   - 2026.11.13 这种日期号（等同三段数字）
//   - 任意后缀（-rc1、+build）在比较时忽略：正式发布只用 tag 名做展示。
//
// 解析失败时退回字符串比较，至少保证按钮在不同版本间有确定行为。
func Compare(a, b string) int {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	if !oka || !okb {
		return strings.Compare(a, b)
	}
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// parseVersion 取数字段。遇到非数字段（如 beta）即停止，避免把预发布后缀当成新版本。
func parseVersion(v string) ([]int, bool) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
	// 去掉 +build 与 -rc 后缀
	if i := strings.IndexAny(v, "+-"); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return nil, false
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			out = append(out, 0)
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			// 段内出现非数字（如 1.0.3abc）：用前缀数字，取不到就整体失败
			digits := strings.TrimRightFunc(p, func(r rune) bool { return r < '0' || r > '9' })
			if digits == "" {
				return nil, false
			}
			n, err = strconv.Atoi(digits)
			if err != nil {
				return nil, false
			}
		}
		out = append(out, n)
	}
	return out, len(out) > 0
}
