package raccoon

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RetryAfter 解析上游给出的「多久之后再试」。优先看响应头，再看响应体。
//
// 上游对 429 的限流窗口有时比本地阶梯长得多：本地一律等 3 分钟，
// 到期后必然再撞一次 429，白白多打一次上游。拿到上游给的重置时刻就能
// 把冷却对齐到真正解除限制的时间。
//
// 顺序与多数网关一致：
//  1. Retry-After（秒数或 HTTP 日期，RFC 7231）
//  2. x-ratelimit-reset / ratelimit-reset（多为秒级时间戳，也见过剩余秒数）
//  3. 响应体：retry_after_ms / retry_after / reset_time / resetTime / reset_at 等
//
// 解析不到或结果不合理（<=0 或超过 24 小时）返回 0，交给本地阶梯兜底。
func RetryAfter(h http.Header, body []byte) time.Duration {
	now := time.Now()
	if d := parseRetryAfterHeader(h.Get("Retry-After"), now); d > 0 {
		return clampRetry(d)
	}
	for _, key := range []string{"X-RateLimit-Reset", "x-ratelimit-reset", "RateLimit-Reset", "ratelimit-reset"} {
		if v := strings.TrimSpace(h.Get(key)); v != "" {
			if d := parseResetValue(v, now); d > 0 {
				return clampRetry(d)
			}
		}
	}
	if len(body) > 0 {
		if d := parseRetryAfterBody(body, now); d > 0 {
			return clampRetry(d)
		}
	}
	return 0
}

// parseRetryAfterHeader 支持 RFC 7231 的两种形态：
//   - 秒数：`Retry-After: 120`
//   - HTTP 日期：`Retry-After: Wed, 21 Oct 2015 07:28:00 GMT`
func parseRetryAfterHeader(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		return time.Duration(secs * float64(time.Second))
	}
	for _, layout := range []string{
		http.TimeFormat, // RFC1123 GMT
		time.RFC1123,    // 带时区名
		time.RFC1123Z,   // 带 +0000
		"Mon, 2 Jan 2006 15:04:05 MST",
	} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.Sub(now)
		}
	}
	return 0
}

// parseResetValue 解析 x-ratelimit-reset 一类头部。它既可能是绝对时间戳
// （秒级 > 1e9、毫秒级 > 1e12），也可能是剩余秒数；按数值大小区分。
func parseResetValue(v string, now time.Time) time.Duration {
	n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0
	}
	switch {
	case n > 1e12: // 毫秒时间戳
		return time.Until(time.UnixMilli(int64(n)))
	case n > 1e9: // 秒时间戳
		return time.Until(time.Unix(int64(n), 0))
	default: // 剩余秒数
		return time.Duration(n * float64(time.Second))
	}
}

// retryAfterKeys 是上游可能用来表达重置时刻的字段名。
// 放在多层级里都要找：顶层、data、error 三种信封形态都出现过。
var retryAfterKeys = []string{
	"retry_after_ms", "retryAfterMs",
	"retry_after", "retryAfter",
	"reset_time", "resetTime",
	"reset_at", "resetAt",
	"next_reset", "nextReset",
}

// parseRetryAfterBody 从响应体里找重置时刻。
//
// 值的形态很杂：剩余毫秒、剩余秒、秒级时间戳、毫秒级时间戳，
// 还有直接给 RFC3339 字符串的。按数值大小判断是时间戳还是时长。
func parseRetryAfterBody(body []byte, now time.Time) time.Duration {
	var root any
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return 0
	}
	return findRetryAfter(root, now, 0)
}

// findRetryAfter 在解析后的 JSON 里深度有限地查找重试字段。
// depth 上限避免异常嵌套把解析拖住。
func findRetryAfter(v any, now time.Time, depth int) time.Duration {
	if depth > 4 {
		return 0
	}
	m, ok := v.(map[string]any)
	if !ok {
		return 0
	}
	for _, key := range retryAfterKeys {
		raw, has := m[key]
		if !has {
			continue
		}
		// 字段名带 Ms 的语义明确是毫秒，不能靠数值大小猜：
		// retry_after_ms=90000 是 90 秒，按「秒」解释会变成 25 小时。
		if strings.Contains(strings.ToLower(key), "ms") {
			if d := millisValue(raw); d > 0 {
				return d
			}
			continue
		}
		if d := valueToDuration(raw, now); d > 0 {
			return d
		}
	}
	// 回退到常见信封的下一层
	for _, key := range []string{"data", "error", "extra"} {
		if child, has := m[key]; has {
			if d := findRetryAfter(child, now, depth+1); d > 0 {
				return d
			}
		}
	}
	return 0
}

// millisValue 只按「毫秒时长」解释一个值（字段名已明确单位）。
func millisValue(raw any) time.Duration {
	switch t := raw.(type) {
	case json.Number:
		if n, err := t.Float64(); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	case float64:
		if t > 0 {
			return time.Duration(t) * time.Millisecond
		}
	case string:
		if n, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return 0
}

// valueToDuration 把一个 JSON 值换算成「从现在起多久」。
func valueToDuration(raw any, now time.Time) time.Duration {
	switch t := raw.(type) {
	case json.Number:
		n, err := t.Float64()
		if err != nil {
			return 0
		}
		return numberToDuration(n, now)
	case float64:
		return numberToDuration(t, now)
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		// RFC3339（上游偶尔直接给 ISO 时间）
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			return ts.Sub(now)
		}
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return numberToDuration(n, now)
		}
	}
	return 0
}

func numberToDuration(n float64, now time.Time) time.Duration {
	switch {
	case n <= 0:
		return 0
	case n > 1e12: // 毫秒时间戳
		return time.Until(time.UnixMilli(int64(n)))
	case n > 1e9: // 秒时间戳
		return time.Until(time.Unix(int64(n), 0))
	case n > 1e6: // 剩余毫秒（大数）
		return time.Duration(n) * time.Millisecond
	default: // 剩余秒
		return time.Duration(n * float64(time.Second))
	}
}

// retryAfterMax 是冷却上限的兜底。上游给一个荒谬的远期时间时，
// 不能让这个号被冻结到下个月；24 小时足够长，和本地阶梯封顶一致。
const retryAfterMax = 24 * time.Hour

func clampRetry(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	if d > retryAfterMax {
		return retryAfterMax
	}
	return d
}
