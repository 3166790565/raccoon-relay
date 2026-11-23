package relay

import (
	"net/http"
	"testing"

	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/raccoon"
)

// 上游用**名字**下发业务码时，分类必须与数字码一致。
//
// 这些码在 json.Unmarshal 到 int 时会失败并留下 0，只看数字码等于「没有错误码」，
// 于是余额耗尽被当成请求错误、限流被当成上游波动。
func TestClassifyNamedCodes(t *testing.T) {
	cases := []struct {
		name     string
		ue       *raccoon.UpstreamError
		wantKind pool.FailKind
		switchTo bool
		wantCode int
	}{
		{
			name:     "insufficient_points 挂在 400",
			ue:       &raccoon.UpstreamError{Status: 400, CodeName: "insufficient_points", Message: "insufficient points"},
			wantKind: pool.FailQuota, switchTo: true, wantCode: http.StatusPaymentRequired,
		},
		{
			name:     "arrearage 挂在 429",
			ue:       &raccoon.UpstreamError{Status: 429, CodeName: "arrearage", Message: "account in arrearage"},
			wantKind: pool.FailQuota, switchTo: true, wantCode: http.StatusPaymentRequired,
		},
		{
			name:     "429 + 中文余额文案",
			ue:       &raccoon.UpstreamError{Status: 429, Message: "积分不足，请充值后重试"},
			wantKind: pool.FailQuota, switchTo: true, wantCode: http.StatusPaymentRequired,
		},
		{
			name:     "429 真限流仍按限流",
			ue:       &raccoon.UpstreamError{Status: 429, CodeName: "rate_limit_exceeded", Message: "too many requests"},
			wantKind: pool.FailRate, switchTo: true, wantCode: http.StatusTooManyRequests,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, switchable, code, msg := classify(c.ue)
			if kind != c.wantKind {
				t.Fatalf("分类应为 %s，实际 %s", c.wantKind, kind)
			}
			if switchable != c.switchTo {
				t.Fatalf("换号应为 %v，实际 %v", c.switchTo, switchable)
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

// 内容策略拦截是「上游对客户端模板句的逐字拦截」，账号本身健康：
// 不冷却、不换号，直接回 400。换号只会白扔另一个号的额度。
func TestClassifyContentPolicy(t *testing.T) {
	for _, msg := range []string{
		"blocked by security policy",
		"unapproved channel",
		"illegal api invocation detected",
	} {
		t.Run(msg, func(t *testing.T) {
			kind, switchable, code, _ := classify(&raccoon.UpstreamError{Status: 400, Message: msg})
			if kind != pool.FailRejected {
				t.Fatalf("应判为内容拒答，实际 %s", kind)
			}
			if switchable {
				t.Fatal("内容拦截不该换号（同一份 body 换谁发都会被拦）")
			}
			if code != http.StatusBadRequest {
				t.Fatalf("对外应回 400，实际 %d", code)
			}
		})
	}
}

// 流内错误帧不再带着伪造的 502：它会让「积分耗尽」被当成上游波动换号，
// 号永远不冷却。留 Status 0 后由业务码与文案分类。
func TestStreamErrorFrameKeepsStatusZero(t *testing.T) {
	cases := []struct {
		name string
		ue   *raccoon.UpstreamError
		want pool.FailKind
	}{
		{"400 + 1000007", &raccoon.UpstreamError{Status: 0, Code: 1000007, Message: "积分余额不足"}, pool.FailQuota},
		{"400 + 敏感词", &raccoon.UpstreamError{Status: 0, Code: 100041, Message: "内容包含敏感内容"}, pool.FailRejected},
		{"无码无文案的裸错误帧", &raccoon.UpstreamError{Status: 0, Message: "上游返回错误帧"}, pool.FailTransient},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if kind, _, _, _ := classify(c.ue); kind != c.want {
				t.Fatalf("应判为 %s，实际 %s", c.want, kind)
			}
		})
	}
}
