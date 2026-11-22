// Package modelmeta 用 models.dev 的社区元数据补齐「上游没声明」的模型能力位
// 与逐模型思考档位。
//
// ── 优先级（本包唯一的规则）─────────────────────────────────
//
//	用户覆盖 > 上游声明 > models.dev > 未声明
//
// 本包只补「上游没声明」的键，从不推翻上游已声明的值：上游那份是「这条转发
// 通道」的真实情况，models.dev 记的是模型在实验室自己那套 API 上的能力，
// 同一模型经不同中转通道能收的东西并不相同。
//
// ── 数据从哪来 ────────────────────────────────────────────
//
//	builtin  仓库里检入的精简快照（modelsdev.json）—— 离线 / 首次启动的地板；
//	cache    统一库里上一次成功刷新的结果（重启不丢）；
//	network  本次联网刷新刚拉到的。
//
// 装载优先级 cache > builtin；刷新成功覆盖，失败保留旧值。
package modelmeta

import (
	"embed"
	"encoding/json"
	"sort"
	"strings"
	"sync"
)

//go:embed modelsdev.json
var embeddedFS embed.FS

// Record 是一条模型元数据（只留网关真正会用到的字段）。
//
// 布尔位与数值都是指针：None = 「这份数据没说」，调用方据此不填，
// 而不是填一个 false（那会凭空断言「不支持」）。
type Record struct {
	Canonical string `json:"canonical"`
	Reasoning *bool  `json:"reasoning,omitempty"`
	ToolCall  *bool  `json:"toolCall,omitempty"`
	Images    *bool  `json:"images,omitempty"`
	Video     *bool  `json:"video,omitempty"`
	Context   *int64 `json:"context,omitempty"`
	Output    *int64 `json:"output,omitempty"`
	// Levels 是该模型认识的命名思考档位（由弱到强）。空 = 这份数据没给出。
	Levels []string `json:"levels,omitempty"`
}

// Snapshot 是一份索引 + 它的来历。
type Snapshot struct {
	Models      map[string]Record
	GeneratedAt int64
	Source      string // builtin / cache / network
}

// Len 收录的模型条数。
func (s *Snapshot) Len() int {
	if s == nil {
		return 0
	}
	return len(s.Models)
}

var (
	mu     sync.RWMutex
	snap   *Snapshot
	loaded bool
)

// KeyOf 是索引键：取 id 的末段，小写、只留字母数字。
//
// 我们各家的模型名基本等同模型本名（glm-5.3 / deepseek-v4.1-flash / kimi-k3），
// 而 models.dev 的 id 一律带 lab 前缀（zhipuai/glm-5.3）；取末段让两侧落进
// 同一个键空间，归一去掉点与连字符后 glm-5.3 / glm-5-3 / GLM-5.3 同键。
func KeyOf(id string) string {
	tail := id
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		tail = id[i+1:]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(tail) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Lookup 按模型 id 查一条元数据（查不到退化为「这个模型 models.dev 没收录」）。
func Lookup(model string) (Record, bool) {
	return LookupIn(Current(), model)
}

// LookupIn 在指定快照里查（测试与刷新链路用）。
func LookupIn(s *Snapshot, model string) (Record, bool) {
	if s == nil {
		return Record{}, false
	}
	key := KeyOf(strings.TrimSpace(model))
	if key == "" {
		return Record{}, false
	}
	rec, ok := s.Models[key]
	return rec, ok
}

// Current 返回当前快照，必要时装载（cache > builtin）。
func Current() *Snapshot {
	mu.RLock()
	if loaded {
		s := snap
		mu.RUnlock()
		return s
	}
	mu.RUnlock()

	mu.Lock()
	defer mu.Unlock()
	if loaded {
		return snap
	}
	snap = LoadEmbedded()
	loaded = true
	return snap
}

// LoadEmbedded 解析内嵌快照。解析失败给一份空索引而不是 panic —— 模型清单
// 本身照常工作，只是所有键都回到「未声明」。
func LoadEmbedded() *Snapshot {
	raw, err := embeddedFS.ReadFile("modelsdev.json")
	if err != nil {
		return &Snapshot{Models: map[string]Record{}, Source: "builtin"}
	}
	if s := Parse(raw, "builtin"); s != nil {
		return s
	}
	return &Snapshot{Models: map[string]Record{}, Source: "builtin"}
}

// Install 用一份快照替换内存索引（启动时先回落到库里那份，刷新成功后再覆盖）。
func Install(s *Snapshot) {
	mu.Lock()
	snap = s
	loaded = true
	mu.Unlock()
}

// Parse 解析一份快照文本（三份来源共用同一形状）。
func Parse(text []byte, source string) *Snapshot {
	var root struct {
		GeneratedAt int64             `json:"generatedAt"`
		Models      map[string]Record `json:"models"`
	}
	if err := json.Unmarshal(text, &root); err != nil {
		return nil
	}
	if len(root.Models) == 0 {
		return nil
	}
	models := make(map[string]Record, len(root.Models))
	for key, rec := range root.Models {
		k := strings.TrimSpace(key)
		if k == "" || rec.Canonical == "" {
			continue
		}
		models[k] = rec
	}
	if len(models) == 0 {
		return nil
	}
	return &Snapshot{Models: models, GeneratedAt: root.GeneratedAt, Source: source}
}

// Serialize 序列化成文件形态（键排序，便于 diff 与逐字比对）。
func Serialize(s *Snapshot) []byte {
	keys := make([]string, 0, len(s.Models))
	for k := range s.Models {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]Record, len(keys))
	for _, k := range keys {
		out[k] = s.Models[k]
	}
	payload := map[string]any{
		"generatedAt": s.GeneratedAt,
		"source":      s.Source,
		"models":      out,
	}
	raw, _ := json.Marshal(payload)
	return raw
}

// Status 是管理页展示的状态（不含任何体积大的内容）。
type Status struct {
	Source    string `json:"source"`
	UpdatedAt int64  `json:"updatedAt"`
	Count     int    `json:"count"`
}

// CurrentStatus 返回当前索引的状态。
func CurrentStatus() Status {
	s := Current()
	if s == nil {
		return Status{}
	}
	return Status{Source: s.Source, UpdatedAt: s.GeneratedAt, Count: s.Len()}
}
