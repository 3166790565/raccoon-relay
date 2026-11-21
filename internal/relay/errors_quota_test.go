package relay

import (
	"net/http"
	"testing"

	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/raccoon"
)

// 1000007「积分余额不足」挂在 HTTP 400 上：必须按业务码认成积分耗尽，
// 换号重试并冷却该号。按状态码分类会落到「不值得换号」的兜底分支。
func TestClassifyPointsInsufficient(t *testing.T) {
	raw := `{"error":{"code":1000007,"message":"积分余额不足，请充值后重试","param":null,"type":"invalid_request_error"}}`
	cases := []struct {
		name   string
		status int
	}{
		{"400（实测形态）", http.StatusBadRequest},
		{"402", http.StatusPaymentRequired},
		{"429", http.StatusTooManyRequests},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, switchable, code, msg := classify(&raccoon.UpstreamError{
				Status: c.status, Code: 1000007, Message: raw,
			})
			if kind != pool.FailQuota {
				t.Fatalf("1000007 应归为积分耗尽，实际 %s", kind)
			}
			if !switchable {
				t.Fatal("积分耗尽应换号重试，把请求交给还有钱的号")
			}
			if code != http.StatusPaymentRequired {
				t.Fatalf("对外应回 402，实际 %d", code)
			}
			if msg == "" {
				t.Fatal("应给出可读文案")
			}
		})
	}
}
