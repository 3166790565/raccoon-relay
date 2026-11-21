package relay

import (
	"net/http"
	"testing"

	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/raccoon"
)

// 请求侧的错误（上下文超长、请求体过大、参数不合法）换号、冷却都不会好：
// 不换号、不冷却，直接回 4xx。它们在流里可能是 HTTP 200 错误帧（Status=0），
// 不显式识别就会掉进「上游波动，换号重试」分支。
func TestClassifyRequestFaults(t *testing.T) {
	cases := []struct {
		name     string
		ue       *raccoon.UpstreamError
		wantCode int
	}{
		{"上下文超长·业务码", &raccoon.UpstreamError{Status: 0, Code: 11115, Message: "context too long"}, http.StatusBadRequest},
		{"上下文超长·文案", &raccoon.UpstreamError{Status: 400, Message: "This model's maximum context length is 128000 tokens"}, http.StatusBadRequest},
		{"请求体过大·状态码", &raccoon.UpstreamError{Status: 413, Message: ""}, http.StatusRequestEntityTooLarge},
		{"Schema 非法", &raccoon.UpstreamError{Status: 0, Code: 4027, Message: "tools.function.parameters: invalid JSON Schema"}, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, switchable, code, msg := classify(c.ue)
			if kind != pool.FailOther {
				t.Fatalf("应为其它（不冷却），实际 %s", kind)
			}
			if switchable {
				t.Fatalf("请求侧错误不该换号重试")
			}
			if code != c.wantCode {
				t.Fatalf("对外状态码应为 %d，实际 %d", c.wantCode, code)
			}
			if msg == "" {
				t.Fatal("应给出可读文案")
			}
		})
	}
}
