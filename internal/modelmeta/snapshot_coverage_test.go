package modelmeta

import "testing"

func TestEmbeddedMainstreamModels(t *testing.T) {
	snapshot := LoadEmbedded()
	for _, id := range []string{"glm-5.3", "deepseek-v4.1-flash", "kimi-k3"} {
		rec, ok := LookupIn(snapshot, id)
		if !ok {
			t.Fatalf("%s 未收录（快照可能过期，可运行 node scripts/sync-modelsdev.mjs）", id)
		}
		if rec.Canonical == "" {
			t.Fatalf("%s 的 canonical 为空", id)
		}
	}
}
