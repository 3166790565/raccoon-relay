package modelmeta

import (
	"context"
	"strings"
	"testing"
)

// 索引键：取末段、小写、只留字母数字。
func TestKeyOf(t *testing.T) {
	cases := map[string]string{
		"zhipuai/glm-5.3":      "glm53",
		"GLM-5.3":              "glm53",
		"deepseek-v4.1-flash":  "deepseekv41flash",
		"anthropic/claude-3.5": "claude35",
		"":                     "",
		"zeta/some-model":      "somemodel",
	}
	for in, want := range cases {
		if got := KeyOf(in); got != want {
			t.Fatalf("KeyOf(%q)=%q，期望 %q", in, got, want)
		}
	}
}

// Reduce 从两份官方 JSON 里抽出能力位与命名档位。
func TestReduceParsesCapabilitiesAndLevels(t *testing.T) {
	models := []byte(`{
	  "zhipuai/glm-5.3": {
	    "type": "chat",
	    "reasoning": true,
	    "tool_call": true,
	    "modalities": {"input": ["text","image"], "output": ["text"]},
	    "limit": {"context": 1000000, "output": 131072}
	  },
	  "some/embedding": {"type": "embedding", "modalities": {"output": ["text"]}}
	}`)
	api := []byte(`{
	  "zhipuai": {"models": {
	    "glm-5.3": {"id": "glm-5.3", "canonical_model_id": "zhipuai/glm-5.3",
	      "reasoning_options": [{"type":"effort","values":["low","medium","high"]}]}
	  }}
	}`)
	snapshot := Reduce(api, models)
	if snapshot == nil {
		t.Fatal("Reduce 返回 nil")
	}
	rec, ok := LookupIn(snapshot, "glm-5.3")
	if !ok {
		t.Fatal("glm-5.3 应被收录")
	}
	if rec.Reasoning == nil || !*rec.Reasoning {
		t.Fatalf("reasoning 未解析: %+v", rec)
	}
	if rec.Images == nil || !*rec.Images {
		t.Fatalf("images 未解析: %+v", rec)
	}
	if rec.Context == nil || *rec.Context != 1000000 || rec.Output == nil || *rec.Output != 131072 {
		t.Fatalf("上下限未解析: %+v", rec)
	}
	if len(rec.Levels) != 3 || rec.Levels[0] != "low" {
		t.Fatalf("档位未解析: %+v", rec.Levels)
	}
	// 非 chat 模型不收录。
	if _, ok := LookupIn(snapshot, "embedding"); ok {
		t.Fatal("embedding 不应收录")
	}
}

// 序列化与解析严格互逆，键排序稳定。
func TestSerializeRoundTrip(t *testing.T) {
	yes := true
	in := &Snapshot{
		GeneratedAt: 123,
		Source:      "network",
		Models: map[string]Record{
			"b": {Canonical: "x/b", ToolCall: &yes, Levels: []string{"low"}},
			"a": {Canonical: "x/a"},
		},
	}
	raw := Serialize(in)
	if !strings.Contains(string(raw), `"a":{"canonical":"x/a"}`) {
		t.Fatalf("键应排序输出: %s", raw)
	}
	out := Parse(raw, "cache")
	if out == nil || out.Len() != 2 || out.Source != "cache" || out.GeneratedAt != 123 {
		t.Fatalf("往返失败: %+v", out)
	}
	if rec, _ := LookupIn(out, "b"); rec.ToolCall == nil || !*rec.ToolCall || len(rec.Levels) != 1 {
		t.Fatalf("记录内容丢失: %+v", rec)
	}
}

// 内嵌快照能解析，且至少收录了主流模型。
func TestEmbeddedSnapshotLoads(t *testing.T) {
	snapshot := LoadEmbedded()
	if snapshot.Len() == 0 {
		t.Fatal("内嵌快照为空（请运行 node scripts/sync-modelsdev.mjs）")
	}
	if snapshot.Source != "builtin" {
		t.Fatalf("来源应为 builtin，实际 %s", snapshot.Source)
	}
}

// Refresh 拉取失败要返回给用户看的原因，且不覆盖现有索引。
func TestRefreshFailureKeepsSnapshot(t *testing.T) {
	before := LoadEmbedded().Len()
	Install(LoadEmbedded())
	_, err := Refresh(context.Background(), func(ctx context.Context, url string) ([]byte, error) {
		return nil, errText("boom")
	})
	if err == nil {
		t.Fatal("拉取失败应返回错误")
	}
	if got := Current().Len(); got != before {
		t.Fatalf("失败不应改动现有索引：before=%d after=%d", before, got)
	}
}

func TestRefreshSuccessInstalls(t *testing.T) {
	api := []byte(`{"zhipuai":{"models":{"glm-5.3":{"id":"glm-5.3","canonical_model_id":"zhipuai/glm-5.3","reasoning_options":[{"type":"effort","values":["low","high"]}]}}}}`)
	models := []byte(`{"zhipuai/glm-5.3":{"type":"chat","reasoning":true,"tool_call":true,"modalities":{"input":["text"],"output":["text"]},"limit":{"context":111,"output":222}}}`)
	snapshot, err := Refresh(context.Background(), func(ctx context.Context, url string) ([]byte, error) {
		if strings.Contains(url, "api.json") {
			return api, nil
		}
		return models, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Len() != 1 {
		t.Fatalf("应收录 1 条，实际 %d", snapshot.Len())
	}
	Install(snapshot)
	rec, ok := Lookup("glm-5.3")
	if !ok || rec.Context == nil || *rec.Context != 111 {
		t.Fatalf("刷新后查询失败: %+v", rec)
	}
}

type errText string

func (e errText) Error() string { return string(e) }
