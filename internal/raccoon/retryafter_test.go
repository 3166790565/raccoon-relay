package raccoon

import (
	"net/http"
	"testing"
	"time"
)

// Retry-After 头的两种形态都要认：秒数与 HTTP 日期。
func TestRetryAfterHeader(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		h    http.Header
		min  time.Duration
		max  time.Duration
	}{
		{"秒数", http.Header{"Retry-After": {"120"}}, 119 * time.Second, 121 * time.Second},
		{"HTTP 日期", http.Header{"Retry-After": {now.Add(90 * time.Second).UTC().Format(http.TimeFormat)}}, 80 * time.Second, 95 * time.Second},
		{"无头", http.Header{}, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RetryAfter(c.h, nil)
			if got < c.min || got > c.max {
				t.Fatalf("期望 %v~%v，实际 %v", c.min, c.max, got)
			}
		})
	}
}

// 响应体里的 reset_time 是秒级时间戳：要换算成「从现在起多久」。
func TestRetryAfterBodyResetTime(t *testing.T) {
	reset := time.Now().Add(5 * time.Minute).Unix()
	body := []byte(`{"error":{"code":6004,"message":"限流","reset_time":` +
		itoa(reset) + `}}`)
	got := RetryAfter(http.Header{}, body)
	if got < 4*time.Minute || got > 6*time.Minute {
		t.Fatalf("应解析出约 5 分钟，实际 %v", got)
	}
}

// retry_after_ms 是剩余毫秒。
func TestRetryAfterBodyMillis(t *testing.T) {
	body := []byte(`{"data":{"retry_after_ms":90000}}`)
	got := RetryAfter(http.Header{}, body)
	if got < 89*time.Second || got > 91*time.Second {
		t.Fatalf("应解析出 90 秒，实际 %v", got)
	}
}

// 荒谬的远期时间要钳到 24 小时以内，避免把号冻结一个月。
func TestRetryAfterClamped(t *testing.T) {
	far := time.Now().Add(90 * 24 * time.Hour).Unix()
	got := RetryAfter(http.Header{}, []byte(`{"reset_time":`+itoa(far)+`}`))
	if got > 24*time.Hour {
		t.Fatalf("应封顶 24 小时，实际 %v", got)
	}
}

// 解析不到时返回 0，交给本地阶梯兜底。
func TestRetryAfterUnknown(t *testing.T) {
	if got := RetryAfter(http.Header{}, []byte(`{"message":"boom"}`)); got != 0 {
		t.Fatalf("无重置信息应返回 0，实际 %v", got)
	}
	if got := RetryAfter(http.Header{"Retry-After": {"not-a-date"}}, nil); got != 0 {
		t.Fatalf("非法值应返回 0，实际 %v", got)
	}
}

// 业务码提取要认多种信封形态，数字字符串也算。
func TestCodeFromBodyShapes(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{`{"code":1000007}`, 1000007},
		{`{"error":{"code":200108}}`, 200108},
		{`{"error_code":710022004}`, 710022004},
		{`{"data":{"code":6004}}`, 6004},
		{`{"code":"11115"}`, 11115},
		{`{"ResponseMetadata":{"Error":{"Code":"11140"}}}`, 11140},
		{`{"message":"boom"}`, 0},
	}
	for _, c := range cases {
		if got := codeFromBody([]byte(c.body)); got != c.want {
			t.Fatalf("%s 应为 %d，实际 %d", c.body, c.want, got)
		}
	}
}

func itoa(n int64) string {
	// 避免额外引入 strconv 到测试命名空间造成重复导入
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf[i:])
}
