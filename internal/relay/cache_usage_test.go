package relay

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
)

// 上游回传 OpenAI 形态的缓存明细时，三条协议都要把它透给下游。
const cachedUsageFrame = `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"prompt_tokens_details":{"cached_tokens":64}}}`

func sseFramesWithUsage(content, usage string) []string {
	frames := sseFrames(content)
	return append(frames, usage)
}

// OpenAI 流式：终帧 usage 带 prompt_tokens_details.cached_tokens。
func TestOpenAIStreamExposesCachedTokens(t *testing.T) {
	h := newHarness(t, []chatResult{{status: 200, frames: sseFramesWithUsage("好", cachedUsageFrame)}})
	h.addAccount(t, "pu-1", 100)

	w := h.do(t, "POST", "/v1/chat/completions",
		`{"model":"sn-test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := w.Body.String()
	if !strings.Contains(body, `"cached_tokens":64`) {
		t.Fatalf("流式响应缺少 cached_tokens：%s", body)
	}
	// prompt_tokens 保持原值（OpenAI 口吻本就含缓存），不能被重复累加。
	if !strings.Contains(body, `"prompt_tokens":100`) {
		t.Fatalf("prompt_tokens 被改写：%s", body)
	}
}

// OpenAI 非流式：同样的字段。
func TestOpenAINonStreamExposesCachedTokens(t *testing.T) {
	h := newHarness(t, []chatResult{{status: 200, frames: sseFramesWithUsage("好", cachedUsageFrame)}})
	h.addAccount(t, "pu-1", 100)

	w := h.do(t, "POST", "/v1/chat/completions",
		`{"model":"sn-test-model","messages":[{"role":"user","content":"hi"}]}`)
	var out struct {
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if out.Usage.PromptTokensDetails.CachedTokens != 64 {
		t.Fatalf("非流式未透出 cached_tokens: %+v", out.Usage)
	}
	if out.Usage.PromptTokens != 100 {
		t.Fatalf("prompt_tokens 不对: %d", out.Usage.PromptTokens)
	}
}

// Anthropic 客户端读的是 cache_read_input_tokens：不输出它就永远算不出命中。
func TestAnthropicExposesCacheReadTokens(t *testing.T) {
	h := newHarness(t, []chatResult{{status: 200, frames: sseFramesWithUsage("好", cachedUsageFrame)}})
	h.addAccount(t, "pu-1", 100)

	w := h.do(t, "POST", "/v1/messages",
		`{"model":"sn-test-model","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := w.Body.String()
	if !strings.Contains(body, `"cache_read_input_tokens":64`) {
		t.Fatalf("Anthropic 流缺少 cache_read_input_tokens：%s", body)
	}
	// Anthropic 口径的 input_tokens 不含缓存：100 - 64 = 36。
	if !strings.Contains(body, `"input_tokens":36`) {
		t.Fatalf("Anthropic 的 input_tokens 应剔除缓存部分：%s", body)
	}
}

// Responses：命中量在 input_tokens_details.cached_tokens。
func TestResponsesExposesCachedTokens(t *testing.T) {
	h := newHarness(t, []chatResult{{status: 200, frames: sseFramesWithUsage("好", cachedUsageFrame)}})
	h.addAccount(t, "pu-1", 100)

	w := h.do(t, "POST", "/v1/responses", `{"model":"sn-test-model","input":"hi"}`)
	var out struct {
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if out.Usage.InputTokensDetails.CachedTokens != 64 {
		t.Fatalf("Responses 未透出 cached_tokens: %+v", out.Usage)
	}
	if out.Usage.InputTokens != 100 {
		t.Fatalf("input_tokens 不对: %d", out.Usage.InputTokens)
	}
}

// 上游不回传缓存字段时，不能凭空输出 cached_tokens：那会被读成「命中 0」。
func TestNoCacheFieldWhenUpstreamSilent(t *testing.T) {
	h := newHarness(t, []chatResult{{status: 200, frames: sseFramesWithUsage("好", usageFrame)}})
	h.addAccount(t, "pu-1", 100)

	w := h.do(t, "POST", "/v1/chat/completions",
		`{"model":"sn-test-model","messages":[{"role":"user","content":"hi"}]}`)
	if strings.Contains(w.Body.String(), "cached_tokens") {
		t.Fatalf("上游没回传时不该伪造缓存字段：%s", w.Body.String())
	}
}

// Anthropic usage 的内部口径转换单独验证，避免依赖整条 HTTP 链路。
func TestAnthropicUsageShape(t *testing.T) {
	u := raccoon.Usage{PromptTokens: 100, CompletionTokens: 5, CachedTokens: 64,
		CacheCreationTokens: 6, CacheReported: true}
	got := anthropicUsage(u)
	if got["input_tokens"] != int64(30) {
		t.Fatalf("input_tokens 应为 100-64-6=30，实际 %v", got["input_tokens"])
	}
	if got["cache_read_input_tokens"] != int64(64) || got["cache_creation_input_tokens"] != int64(6) {
		t.Fatalf("缓存字段不对: %v", got)
	}
	// 未回传时不输出缓存字段
	silent := anthropicUsage(raccoon.Usage{PromptTokens: 10, CompletionTokens: 1})
	if _, ok := silent["cache_read_input_tokens"]; ok {
		t.Fatal("未回传时不该输出 cache_read_input_tokens")
	}
	if silent["input_tokens"] != int64(10) {
		t.Fatalf("未回传时 input_tokens 应原样，实际 %v", silent["input_tokens"])
	}
}
