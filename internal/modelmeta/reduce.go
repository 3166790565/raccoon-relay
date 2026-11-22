package modelmeta

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// URLs 是 models.dev 的两份官方端点。
const (
	APIURL    = "https://models.dev/api.json"
	ModelsURL = "https://models.dev/models.json"
)

// Reduce 把两份官方 JSON 压成精简索引（与 scripts/sync-modelsdev 同口径：
// 内嵌快照就是它产出的，运行期刷新必须逐字复现同一套规则）。
//
//   - models.json 负责 canonical 条目的能力位与上下限；
//   - api.json 负责 provider 条目的命名思考档位（lab 家自述的那条优先）。
func Reduce(api, models []byte) *Snapshot {
	var canonical map[string]map[string]any
	if err := json.Unmarshal(models, &canonical); err != nil {
		return nil
	}
	var providers map[string]struct {
		Models map[string]map[string]any `json:"models"`
	}
	if err := json.Unmarshal(api, &providers); err != nil {
		return nil
	}

	index := make(map[string]Record, len(canonical))
	for canonicalID, entry := range canonical {
		if !isChatModel(entry) {
			continue
		}
		key := KeyOf(canonicalID)
		if key == "" {
			continue
		}
		input := stringList(entry, "modalities", "input")
		index[key] = Record{
			Canonical: canonicalID,
			Reasoning: boolPtr(entry["reasoning"]),
			ToolCall:  boolPtr(entry["tool_call"]),
			Images:    boolPtr(containsStr(input, "image")),
			Video:     boolPtr(containsStr(input, "video")),
			Context:   limitOf(nested(entry, "limit", "context")),
			Output:    limitOf(nested(entry, "limit", "output")),
		}
	}

	// 档位：lab 家自述的那条优先；都声明了按 provider id 字典序取第一条。
	type pick struct {
		levels []string
		isLab  bool
	}
	picks := map[string]pick{}
	providerIDs := make([]string, 0, len(providers))
	for id := range providers {
		providerIDs = append(providerIDs, id)
	}
	sort.Strings(providerIDs)
	for _, providerID := range providerIDs {
		provider := providers[providerID]
		for _, entry := range provider.Models {
			canonicalID, _ := entry["canonical_model_id"].(string)
			if canonicalID == "" {
				canonicalID, _ = entry["id"].(string)
			}
			key := KeyOf(canonicalID)
			if key == "" {
				continue
			}
			levels := effortLevels(entry)
			if len(levels) == 0 {
				continue
			}
			isLab := labOf(canonicalID) == providerID
			prev, seen := picks[key]
			if seen && (prev.isLab || !isLab) {
				continue
			}
			picks[key] = pick{levels: levels, isLab: isLab}
		}
	}
	for key, p := range picks {
		if rec, ok := index[key]; ok {
			rec.Levels = p.levels
			index[key] = rec
		}
	}

	if len(index) == 0 {
		return nil
	}
	return &Snapshot{Models: index, GeneratedAt: time.Now().UnixMilli(), Source: "network"}
}

// isChatModel 只收录能出文本的对话模型（embedding / reranking / 生图生视频都不要）。
func isChatModel(entry map[string]any) bool {
	if kind, ok := entry["type"].(string); ok && kind != "" && kind != "chat" {
		return false
	}
	if output := stringList(entry, "modalities", "output"); len(output) > 0 {
		return containsStr(output, "text")
	}
	return true
}

// effortLevels 读 provider 条目的命名档位；只有 effort 形态算数
// （toggle / 空数组 = 没给命名档位，网关侧回落通用表）。
func effortLevels(entry map[string]any) []string {
	options, _ := entry["reasoning_options"].([]any)
	for _, raw := range options {
		option, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := option["type"].(string); kind != "effort" {
			continue
		}
		values, _ := option["values"].([]any)
		out := make([]string, 0, len(values))
		for _, v := range values {
			if s, ok := v.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// limitOf 只接受正数（0 / 负数 / 小数 / 非数字 → nil = 「这份数据没说」）。
func limitOf(v any) *int64 {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case json.Number:
		parsed, err := n.Float64()
		if err != nil {
			return nil
		}
		f = parsed
	case int64:
		f = float64(n)
	case int:
		f = float64(n)
	default:
		return nil
	}
	if f < 1 {
		return nil
	}
	n := int64(f)
	return &n
}

// nested 取 entry 的多层字段：entry["a"].(map)["b"]。
func nested(entry map[string]any, path ...string) any {
	var cur any = entry
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[key]
	}
	return cur
}

func stringList(entry map[string]any, path ...string) []string {
	raw, _ := nested(entry, path...).([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func labOf(canonical string) string {
	if i := strings.IndexByte(canonical, '/'); i > 0 {
		return canonical[:i]
	}
	return ""
}

func boolPtr(v any) *bool {
	if b, ok := v.(bool); ok {
		return &b
	}
	return nil
}
