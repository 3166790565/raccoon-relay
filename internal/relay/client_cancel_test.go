package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
)

// 回归：客户端在流式响应中途断开，不能被当成上游故障。
//
// 线上原始症状（面板流水）：状态 502，错误「上游流读取中断: context canceled」，
// 用时五到七秒、输入输出都是 0。请求方其实只是点了停止或关掉页面。
//
// 旧实现有两个毛病叠在一起：
//  1. raccoon.ChatStream 把 context.Canceled 包进 UpstreamError 时丢掉了
//     sentinel（没有 Unwrap），relay.isClientGone 认不出来；
//  2. 流式 sink 写出首帧后 at.Started=true，引擎不再换号，直接把这个
//     UpstreamError 交给 classify → 落进「Status 0 → 上游波动」→ 502。
//
// 因此断言必须落在「客户端断开识别」与「对外状态码」上。
// 不能断言退避：客户端断开时请求 ctx 已被取消，退避写库本身也会失败。
func TestClientCancelMidStreamIsNotUpstreamFault(t *testing.T) {
	h := newHarness(t, []chatResult{{
		frames: []string{`{"choices":[{"delta":{"content":"半截"}}]}`},
		// 吐一帧后一直挂着：等客户端断开，模拟 App 里点「停止」。
		hangAfterFrames: true,
	}})
	h.addAccount(t, "pu-1", 100)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 用真实的流式 sink：它实现 Commitment()，写出首帧后引擎就锁定这个号，
	// 与线上 SSE 路径完全一致（否则引擎会换号重试，掩盖掉问题）。
	sink := &cancelAfterFirstDelta{
		inner:  newSSEWriter(httptest.NewRecorder(), "sn-test-model"),
		cancel: cancel,
	}

	cc := BuildChatContext("sn-test-model", map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, DefaultModel, "")

	_, err := h.engine.RunOpenAI(ctx, cc, sink)
	if err == nil {
		t.Fatal("客户端断开后应返回错误")
	}

	// 1) 必须被识别成「客户端已断开」，而不是上游故障。
	if !isClientGone(err) {
		t.Fatalf("isClientGone 应认出被包装过的取消错误，实际：%v（类型 %T）", err, err)
	}

	// 2) 对外状态码必须是 499，不能是 502 —— 这正是截图里记错的那一项。
	if code, _ := publicError(err); code != 499 {
		t.Fatalf("客户端取消对外应为 499，实际 %d（错误：%v）", code, err)
	}
}

// cancelAfterFirstDelta 包装真实 SSE sink：收到第一段正文就取消请求上下文。
type cancelAfterFirstDelta struct {
	inner  *sseWriter
	cancel context.CancelFunc
	fired  bool
}

func (c *cancelAfterFirstDelta) Begin() error { return c.inner.Begin() }

func (c *cancelAfterFirstDelta) Delta(d raccoon.Delta) error {
	if err := c.inner.Delta(d); err != nil {
		return err
	}
	if !c.fired {
		c.fired = true
		c.cancel()
	}
	return nil
}

func (c *cancelAfterFirstDelta) Usage(u raccoon.Usage) { c.inner.Usage(u) }
func (c *cancelAfterFirstDelta) Finish(r string)       { c.inner.Finish(r) }
func (c *cancelAfterFirstDelta) Complete()             { c.inner.Complete() }

// Commitment 转发给内层：Begin 之后即视为已写出，引擎不再换号。
func (c *cancelAfterFirstDelta) Commitment() bool { return c.inner.Commitment() }

// 上游静默超时仍是上游侧的问题：文案要指向上游，且不能伪装成客户端断开。
func TestUpstreamIdleTimeoutIsNotClientGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		fl.Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer srv.Close()

	c := raccoon.NewClient(srv.URL, 0)
	c.IdleTimeout = 80 * time.Millisecond
	stream, err := c.OpenChat(context.Background(), raccoon.ChatRequest{Body: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	if _, err := stream.Next(); err != nil {
		t.Fatalf("首帧: %v", err)
	}
	_, err = stream.Next()
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("应收到空闲超时错误，实际 %v", err)
	}
	if isClientGone(err) {
		t.Fatalf("上游静默不能被判成客户端断开：%v", err)
	}
	if !strings.Contains(err.Error(), "无数据") {
		t.Fatalf("文案应指向上游静默，实际 %v", err)
	}
}

// 客户端在建连阶段就取消，同样要保留 sentinel。
func TestClientCancelBeforeHeadersIsClientGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer srv.Close()

	c := raccoon.NewClient(srv.URL, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(50*time.Millisecond, cancel)

	_, err := c.OpenChat(ctx, raccoon.ChatRequest{Body: map[string]any{}})
	if err == nil {
		t.Fatal("取消后应返回错误")
	}
	if !isClientGone(err) {
		t.Fatalf("建连阶段取消应识别为客户端断开，实际：%v", err)
	}
}
