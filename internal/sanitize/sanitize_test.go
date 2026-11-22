package sanitize

import (
	"strings"
	"testing"
	"time"
)

func timeAfter() <-chan time.Time { return time.After(2 * time.Second) }

// 内置词表必须始终可编译（发版下限）。
func TestDefaultRuleSetCompiles(t *testing.T) {
	rs := DefaultRuleSet()
	if len(rs.Entries()) == 0 {
		t.Fatal("内置词表不应为空")
	}
}

// 字面替换命中并以「原文」为标签计数。
func TestLiteralRuleReplaces(t *testing.T) {
	rs, err := Compile([]Rule{{
		ID: "cc", Name: "Claude Code", Enabled: true, MatchType: MatchLiteral,
		Pattern: "Anthropic's official CLI for Claude", Replacement: "Anthropic's official CLI tool for Claude",
	}})
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"messages": []any{map[string]any{
			"role": "system",
			"content": "You are Claude Code, Anthropic's official CLI for Claude. " +
				"Anthropic's official CLI for Claude says hi.",
		}},
	}
	result := rs.SanitizeRequest(body)
	if !result.Changed {
		t.Fatal("应命中并改写")
	}
	got := result.Body["messages"].([]any)[0].(map[string]any)["content"].(string)
	if strings.Contains(got, "official CLI for Claude") {
		t.Fatalf("未替换干净: %q", got)
	}
	if len(result.Hits) != 1 || result.Hits[0].Count != 2 {
		t.Fatalf("命中计数不对: %+v", result.Hits)
	}
	// 原请求体不能被改动（只改副本）。
	orig := body["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(orig, "official CLI for Claude") {
		t.Fatal("原始请求体被意外改写")
	}
}

// 正则替换使用字面替换（替换内容里的 $1 不当捕获组）。
func TestRegexReplacementIsLiteral(t *testing.T) {
	rs, err := Compile([]Rule{{
		ID: "dollar", Name: "dollar", Enabled: true, MatchType: MatchRegex,
		Pattern: `foo\d+`, Replacement: "bar$1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	text, hits := rs.SanitizeText("foo123 baz", TextScope{Role: "user", Content: true})
	if text != "bar$1 baz" {
		t.Fatalf("替换内容应原样插入，实际 %q", text)
	}
	if len(hits) != 1 || hits[0].Count != 1 {
		t.Fatalf("命中不对: %+v", hits)
	}
}

// 指定角色时只改该角色的正文；辅助字段不碰。
func TestRoleScopedRules(t *testing.T) {
	rs, err := Compile([]Rule{{
		ID: "brand", Name: "brand", Enabled: true, MatchType: MatchLiteral,
		Pattern: "DeepSeek Harness", Replacement: "DeepSeek", Roles: []string{"system"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "DeepSeek Harness says"},
			map[string]any{"role": "user", "content": "DeepSeek Harness is great"},
		},
	}
	result := rs.SanitizeRequest(body)
	if !result.Changed {
		t.Fatal("system 消息应命中")
	}
	msgs := result.Body["messages"].([]any)
	if msgs[0].(map[string]any)["content"] == "DeepSeek Harness says" {
		t.Fatal("system 消息未改写")
	}
	if msgs[1].(map[string]any)["content"] != "DeepSeek Harness is great" {
		t.Fatal("user 消息不应被规则作用于")
	}
}

// 内容块数组只改 text 块，图片原样保留。
func TestContentParts(t *testing.T) {
	rs, err := Compile([]Rule{{
		ID: "x", Name: "x", Enabled: true, MatchType: MatchLiteral,
		Pattern: "secret", Replacement: "[redacted]",
	}})
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "my secret"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,secret"}},
			},
		}},
	}
	result := rs.SanitizeRequest(body)
	if !result.Changed {
		t.Fatal("应命中 text 块")
	}
	parts := result.Body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if parts[0].(map[string]any)["text"] != "my [redacted]" {
		t.Fatalf("text 块未改写: %+v", parts[0])
	}
	img := parts[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if img != "data:image/png;base64,secret" {
		t.Fatal("图片 URL 不应被规则改写")
	}
}

// 非法规则整体拒绝：越界、重复 id、坏正则、重复清理配替换。
func TestCompileRejectsInvalid(t *testing.T) {
	cases := []Rule{
		{ID: "id with spaces", Name: "a", Enabled: true, MatchType: MatchLiteral, Pattern: "x"},
		{ID: "", Name: "b", Enabled: true, MatchType: MatchLiteral, Pattern: "x"},
		{ID: "bad-regex", Name: "c", Enabled: true, MatchType: MatchRegex, Pattern: "("},
		{ID: "repeat", Name: "d", Enabled: true, MatchType: MatchRegex, Pattern: "x", Replacement: "y", Repeat: true},
	}
	for _, rule := range cases {
		if _, err := Compile([]Rule{rule}); err == nil {
			t.Fatalf("非法规则应被拒绝: %+v", rule)
		}
	}
	// 重复 id 也要拒绝。
	dup := Rule{ID: "same", Name: "a", Enabled: true, MatchType: MatchLiteral, Pattern: "x"}
	if _, err := Compile([]Rule{dup, dup}); err == nil {
		t.Fatal("重复 id 应被拒绝")
	}
	// 超过上限也拒绝。
	var many []Rule
	for i := 0; i <= MaxRules; i++ {
		many = append(many, Rule{ID: "r" + itoa(i), Name: "n", Enabled: true, MatchType: MatchLiteral, Pattern: "x"})
	}
	if _, err := Compile(many); err == nil {
		t.Fatal("超过上限应被拒绝")
	}
}

// 空词表 = 不改动，返回 Changed=false 且不复制。
func TestEmptyRuleSetIsNoop(t *testing.T) {
	rs, err := Compile(nil)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	if result := rs.SanitizeRequest(body); result.Changed {
		t.Fatal("空词表不应改动")
	}
}

// 删除规则（例如清掉 cc_ 参数）也要记命中，便于流水解释「文本怎么变短了」。
func TestDeletionRecordsHit(t *testing.T) {
	rs, err := Compile([]Rule{{
		ID: "strip", Name: "strip", Enabled: true, MatchType: MatchRegex,
		Pattern: `(?i)\bcc_[a-z0-9_]+=[^;\n ]*;?[ \t]*`, Replacement: "",
	}})
	if err != nil {
		t.Fatal(err)
	}
	text, hits := rs.SanitizeText("Hello cc_hello=world after", TextScope{Role: "system", Content: true})
	if strings.TrimSpace(text) != "Hello after" || len(hits) != 1 || hits[0].Count != 1 {
		t.Fatalf("删除规则未生效: text=%q hits=%+v", text, hits)
	}
}

// 零宽匹配不应无限循环（Repeat 打开也一样）。
func TestZeroWidthRepeatTerminates(t *testing.T) {
	rs, err := Compile([]Rule{{
		ID: "zero", Name: "zero", Enabled: true, MatchType: MatchRegex,
		Pattern: `x*`, Replacement: "", Repeat: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_, _ = rs.SanitizeText("abc", TextScope{Role: "user", Content: true})
		close(done)
	}()
	select {
	case <-done:
	case <-timeAfter():
		t.Fatal("零宽匹配导致死循环")
	}
}

// 内置词表里的身份改写确实能命中 Claude Code 指纹（防止默认规则失效）。
func TestDefaultRulesCatchClientFingerprints(t *testing.T) {
	rs := DefaultRuleSet()
	text := "You are Claude Code, Anthropic's official CLI for Claude, " +
		"and you are powered by Anthropic."
	out, hits := rs.SanitizeText(text, TextScope{Role: "system", Content: true})
	if out == text || len(hits) == 0 {
		t.Fatalf("内置词表未命中: %q", out)
	}
}
