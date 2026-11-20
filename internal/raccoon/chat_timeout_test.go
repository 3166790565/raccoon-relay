package raccoon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 回归：整条流的总时长远超超时值，但上游一直在出数据，不能被掐断。
// 旧实现用 http.Client.Timeout 管流式，这里会报 context deadline exceeded。
func TestStreamLongerThanTimeoutButActive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 8; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
			fl.Flush()
			time.Sleep(50 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 0)
	c.IdleTimeout = 150 * time.Millisecond

	stream, err := c.OpenChat(context.Background(), ChatRequest{Body: map[string]any{}})
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	defer stream.Close()

	n := 0
	for {
		ev, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if ev.Delta != nil {
			n++
		}
	}
	if n != 8 {
		t.Fatalf("收到 %d 帧，期望 8", n)
	}
}

// 上游静默超过空闲阈值时，要中断并给出可读文案，而不是一直挂着。
func TestStreamIdleTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
		fl.Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 0)
	c.IdleTimeout = 120 * time.Millisecond

	stream, err := c.OpenChat(context.Background(), ChatRequest{Body: map[string]any{}})
	if err != nil {
		t.Fatalf("OpenChat: %v", err)
	}
	defer stream.Close()

	if _, err := stream.Next(); err != nil {
		t.Fatalf("首帧: %v", err)
	}
	start := time.Now()
	_, err = stream.Next()
	if err == nil || err == io.EOF {
		t.Fatalf("期望空闲超时错误，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "无数据") {
		t.Fatalf("错误文案不直观: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("空闲超时生效太慢: %v", elapsed)
	}
}
