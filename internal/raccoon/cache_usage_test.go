package raccoon

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

// OpenAI 形态：缓存命中在 prompt_tokens_details.cached_tokens，
// prompt_tokens 本身已含缓存。
func TestUsageFromFrameOpenAI(t *testing.T) {
	u := &streamUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}
	u.PromptTokensDetails = &struct {
		CachedTokens int64 `json:"cached_tokens"`
	}{CachedTokens: 80}

	out := usageFromFrame(u)
	if !out.CacheReported {
		t.Fatal("上游给了缓存字段，应标记为已回传")
	}
	if out.CachedTokens != 80 {
		t.Fatalf("命中量应为 80，实际 %d", out.CachedTokens)
	}
	if out.PromptTokens != 100 {
		t.Fatalf("OpenAI 口吻的 prompt_tokens 已含缓存，不该再加，实际 %d", out.PromptTokens)
	}
}

// Anthropic 形态：input_tokens **不含**缓存，需求归一到 OpenAI 口径。
func TestUsageFromFrameAnthropic(t *testing.T) {
	read, created := int64(60), int64(10)
	u := &streamUsage{InputTokens: 30, OutputTokens: 5, CacheReadInputTokens: &read, CacheCreationInputTokens: &created}
	u.PromptTokensDetails = nil

	out := usageFromFrame(u)
	if !out.CacheReported {
		t.Fatal("Anthropic 给了缓存字段，应标记为已回传")
	}
	if out.CachedTokens != 60 || out.CacheCreationTokens != 10 {
		t.Fatalf("缓存量解析不对: read=%d create=%d", out.CachedTokens, out.CacheCreationTokens)
	}
	// 30 是没命中的部分，加上 60 读 + 10 写 = 全部输入 100。
	if out.PromptTokens != 100 {
		t.Fatalf("输入应归一到含缓存的 100，实际 %d", out.PromptTokens)
	}
}

// 上游不传缓存字段时，CachedTokens 为 0 但必须标记为「未回传」，
// 否则下游会把它当成「命中 0」而伪造出 0% 命中率。
func TestUsageFromFrameNoCacheFields(t *testing.T) {
	out := usageFromFrame(&streamUsage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13})
	if out.CacheReported {
		t.Fatal("上游没给缓存字段，不应标记为已回传")
	}
	if out.CachedTokens != 0 {
		t.Fatalf("未回传时命中量应为 0，实际 %d", out.CachedTokens)
	}
}

// OpenAI 兼容上游同时下发 cache_read_input_tokens 时不能重复累加：
// prompt_tokens 里已经算过了。
func TestUsageFromFrameOpenAIWithCacheRead(t *testing.T) {
	read := int64(40)
	out := usageFromFrame(&streamUsage{PromptTokens: 100, InputTokens: 100,
		CompletionTokens: 10, CacheReadInputTokens: &read})
	if out.PromptTokens != 100 {
		t.Fatalf("已有 prompt_tokens 时不该再加缓存读，实际 %d", out.PromptTokens)
	}
	if !out.CacheReported || out.CachedTokens != 40 {
		t.Fatalf("缓存命中仍应记录: reported=%v cached=%d", out.CacheReported, out.CachedTokens)
	}
}

// 真流：OpenAI 形态的终帧要被解析出缓存明细。
func TestChatStreamParsesCachedTokens(t *testing.T) {
	frames := []string{
		`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":50,"completion_tokens":5,"total_tokens":55,"prompt_tokens_details":{"cached_tokens":32}}}`,
	}
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: " + f + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	s := NewChatStream(nil, bufio.NewScanner(io.NopCloser(strings.NewReader(b.String()))),
		func() {}, nil, 0)
	defer s.Close()

	var got *Usage
	for {
		ev, err := s.Next()
		if err != nil {
			break
		}
		if ev.Usage != nil {
			got = ev.Usage
		}
	}
	if got == nil {
		t.Fatal("应解析出 usage")
	}
	if !got.CacheReported || got.CachedTokens != 32 {
		t.Fatalf("缓存明细未解析: reported=%v cached=%d", got.CacheReported, got.CachedTokens)
	}
}
