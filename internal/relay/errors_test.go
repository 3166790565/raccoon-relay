package relay

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/raccoon"
)

// 403 不等于凭证过期：只有文案明确指向登录态时才判 relogin。
func TestClassify403(t *testing.T) {
	cases := []struct {
		name     string
		msg      string
		wantKind pool.FailKind
		wantCode int
	}{
		{"空文案按风控", "", pool.FailBlocked, http.StatusForbidden},
		{"权限不足按风控", "permission denied", pool.FailBlocked, http.StatusForbidden},
		{"请求过期不等于登录过期", "请求已过期", pool.FailBlocked, http.StatusForbidden},
		{"签名过期", "signature expired", pool.FailBlocked, http.StatusForbidden},
		{"登录态失效才算重登", "登录态已失效，请重新登录", pool.FailRelogin, http.StatusUnauthorized},
		{"authorization_verify_error 算重登", "authorization_verify_error", pool.FailRelogin, http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, switchable, code, msg := classify(&raccoon.UpstreamError{Status: http.StatusForbidden, Message: c.msg})
			if kind != c.wantKind {
				t.Fatalf("分类应为 %s，实际 %s", c.wantKind, kind)
			}
			if code != c.wantCode {
				t.Fatalf("对外状态码应为 %d，实际 %d", c.wantCode, code)
			}
			if !switchable {
				t.Fatal("403 应允许换号重试")
			}
			if msg == "" {
				t.Fatal("应给出可读文案")
			}
		})
	}
}

// 带 Auth 标记的 401 仍然是 relogin（refresh_token 已经不可用）。
func TestClassify401Auth(t *testing.T) {
	kind, _, code, _ := classify(&raccoon.UpstreamError{Status: http.StatusUnauthorized, Auth: true, Message: "凭证失效"})
	if kind != pool.FailRelogin || code != http.StatusUnauthorized {
		t.Fatalf("401+Auth 应为 relogin/401，实际 %s/%d", kind, code)
	}
}

// 强刷策略：只有「access 过期/不认」才值得刷，普通 403 不刷。
func TestShouldRefreshRetry(t *testing.T) {
	future := time.Now().Add(time.Hour).UnixMilli()
	past := time.Now().Add(-time.Hour).UnixMilli()

	cases := []struct {
		name string
		err  *raccoon.UpstreamError
		exp  int64
		want bool
	}{
		{"401", &raccoon.UpstreamError{Status: 401, Message: "unauthorized"}, future, true},
		{"Auth 标记", &raccoon.UpstreamError{Status: 403, Auth: true}, future, true},
		{"普通 403 + 未过期", &raccoon.UpstreamError{Status: 403, Message: "permission denied"}, future, false},
		{"403 文案说是登录态", &raccoon.UpstreamError{Status: 403, Message: "登录态已失效，请重新登录"}, future, true},
		{"403 + 本地 exp 已过期", &raccoon.UpstreamError{Status: 403, Message: "permission denied"}, past, true},
		{"402 不刷", &raccoon.UpstreamError{Status: 402, Message: "积分不足"}, future, false},
		{"429 不刷", &raccoon.UpstreamError{Status: 429, Message: "限流"}, future, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldRefreshRetry(c.err, c.exp); got != c.want {
				t.Fatalf("期望 %v，实际 %v", c.want, got)
			}
		})
	}
}

// 403 对外文案不能让人误以为是账号过期。
func TestBlockedMessageNotMisleading(t *testing.T) {
	msg := blockedMessage("")
	if msg == "" || msg == "上游请求失败" {
		t.Fatalf("空文案应替换成 403 说明，实际 %q", msg)
	}
}

// 200108 是账号级处罚，必须按 code 认出来，不能只看 HTTP 状态。
func TestClassifyAccountAbnormal(t *testing.T) {
	for _, status := range []int{429, 400, 403} {
		kind, switchable, code, msg := classify(&raccoon.UpstreamError{
			Status: status, Code: 200108, Message: "系统检测到当前账号存在异常调用",
		})
		if kind != pool.FailAccountAbnormal {
			t.Fatalf("HTTP %d + code 200108 应归为账号异常，实际 %s", status, kind)
		}
		if !switchable {
			t.Fatal("账号异常应允许换号重试，把请求交给别的号")
		}
		if code != http.StatusTooManyRequests {
			t.Fatalf("对外应回 429（可稍后重试），实际 %d", code)
		}
		if msg == "" {
			t.Fatal("应给出可读文案")
		}
	}
}

// 客户端取消与写回失败不能被当成上游故障。
func TestClassifyClientGone(t *testing.T) {
	cases := []error{
		context.Canceled,
		errors.New("write tcp: broken pipe"),
		errClientGone,
	}
	for _, err := range cases {
		kind, switchable, code, _ := classify(err)
		if kind != pool.FailOther {
			t.Fatalf("%v 应按客户端断开处理，实际 %s", err, kind)
		}
		if switchable {
			t.Fatalf("%v 不该继续换号重试", err)
		}
		if code != 499 {
			t.Fatalf("%v 对外应为 499，实际 %d", err, code)
		}
		if !isClientGone(err) {
			t.Fatalf("%v 应被 isClientGone 识别", err)
		}
	}
	if isClientGone(&raccoon.UpstreamError{Status: 500, Message: "上游炸了"}) {
		t.Fatal("上游 5xx 不是客户端断开")
	}
}

// 200108 的英文/中文原文不参与分类，只认 code；但消息必须是可读的。
func TestAccountAbnormalMessageReadable(t *testing.T) {
	_, _, _, msg := classify(&raccoon.UpstreamError{Status: 429, Code: 200108, Message: "账号异常"})
	if msg != "账号异常" {
		t.Fatalf("应保留上游原文，实际 %q", msg)
	}
}
