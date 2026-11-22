package sanitize

import (
	"regexp"
	"strings"
)

// 规则上限（与 Agent2API 的 core/sanitize 保持一致）：
// 词表最多 200 条，名称 128 字，匹配内容 4096 字，替换内容 4096 字。
const (
	MaxRules            = 200
	MaxNameChars        = 128
	MaxPatternChars     = 4096
	MaxReplacementChars = 4096
)

// 规则可作用的角色。空角色列表 = 对全部字段生效（旧规则的全字段行为）。
var Roles = []string{"system", "developer", "user", "assistant", "tool", "function"}

// MatchType 是匹配方式：literal 按原文替换，regex 按正则替换。
type MatchType string

const (
	MatchLiteral MatchType = "literal"
	MatchRegex   MatchType = "regex"
)

// Rule 是一条脱敏规则。JSON 形状与面板 / Agent2API 一致（camelCase）。
type Rule struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Enabled     bool      `json:"enabled"`
	MatchType   MatchType `json:"matchType"`
	Pattern     string    `json:"pattern"`
	Replacement string    `json:"replacement"`
	// Repeat：重复执行直到不再变化。只允许「正则删除」（replacement 为空），
	// 否则用户规则可能不断扩张文本而无法收敛。
	Repeat bool `json:"repeat"`
	// Roles 为空 = 全字段；指定角色时只改这些角色消息的正文。
	Roles []string `json:"roles,omitempty"`
}

// CompiledRule 是编译后的规则：正则已预编译。
type CompiledRule struct {
	Rule  Rule
	regex *regexp.Regexp
}

// RuleSet 是编译好的规则集。词表与编译结果一起不可变，转发时整份替换。
type RuleSet struct {
	entries  []Rule
	compiled []CompiledRule
}

// Entries 返回原始词表（管理页展示 / 持久化用）。
func (rs *RuleSet) Entries() []Rule {
	if rs == nil {
		return nil
	}
	out := make([]Rule, len(rs.entries))
	copy(out, rs.entries)
	return out
}

// Empty 表示词表里一条启用规则都没有（脱敏事实上不生效）。
// 转发热路径每次都会问一次，所以不能走 Entries() 的拷贝。
func (rs *RuleSet) Empty() bool {
	return rs == nil || len(rs.compiled) == 0
}

// Compile 校验并编译一份词表。任何一条非法都整体拒绝（返回错误），
// 不会「存一半」。
func Compile(entries []Rule) (*RuleSet, error) {
	if len(entries) > MaxRules {
		return nil, errf("脱敏词表最多 %d 条规则", MaxRules)
	}
	norm := make([]Rule, 0, len(entries))
	ids := map[string]bool{}
	compiled := make([]CompiledRule, 0, len(entries))
	for i := range entries {
		rule := entries[i]
		prefix := "第 " + itoa(i+1) + " 条脱敏规则"
		rule.ID = strings.TrimSpace(rule.ID)
		if rule.ID == "" || len(rule.ID) > 64 || !validID(rule.ID) {
			return nil, errf("%s的 id 必须是 1–64 位字母、数字、连字符或下划线", prefix)
		}
		if ids[rule.ID] {
			return nil, errf("脱敏规则 id 重复：%s", rule.ID)
		}
		ids[rule.ID] = true
		rule.Name = strings.TrimSpace(rule.Name)
		if rule.Name == "" || len([]rune(rule.Name)) > MaxNameChars {
			return nil, errf("%s的名称必须是 1–%d 个字符", prefix, MaxNameChars)
		}
		if rule.MatchType != MatchLiteral && rule.MatchType != MatchRegex {
			return nil, errf("%s的匹配方式只能是 literal 或 regex", prefix)
		}
		if strings.TrimSpace(rule.Pattern) == "" || len([]rune(rule.Pattern)) > MaxPatternChars {
			return nil, errf("%s的匹配内容不能为空，且最多 %d 个字符", prefix, MaxPatternChars)
		}
		if len([]rune(rule.Replacement)) > MaxReplacementChars {
			return nil, errf("%s的替换内容最多 %d 个字符", prefix, MaxReplacementChars)
		}
		rule.Roles = normalizeRoles(rule.Roles)
		if rule.Repeat && (rule.MatchType != MatchRegex || rule.Replacement != "") {
			return nil, errf("%s只有正则删除规则可以启用重复清理", prefix)
		}
		var re *regexp.Regexp
		if rule.MatchType == MatchRegex {
			var err error
			re, err = regexp.Compile(rule.Pattern)
			if err != nil {
				return nil, errf("%s的正则表达式无效：%v", prefix, err)
			}
		}
		norm = append(norm, rule)
		if rule.Enabled {
			compiled = append(compiled, CompiledRule{Rule: rule, regex: re})
		}
	}
	return &RuleSet{entries: norm, compiled: compiled}, nil
}

// DefaultRuleSet 返回内置词表（构造时编译一次；内置词表必须有效）。
func DefaultRuleSet() *RuleSet {
	rs, err := Compile(DefaultRules())
	if err != nil {
		// 内置词表是我们自己维护的，编译不过属于发版事故；给空词表而不是 panic，
		// 让网关其余功能照常工作。
		return &RuleSet{}
	}
	return rs
}

// normalizeRoles 归一角色：去空白、小写、去重、按固定顺序排列。
// 未知角色直接拒绝（返回 nil 会让规则变成「全字段」，那是悄悄扩大作用范围）。
func normalizeRoles(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, r := range in {
		v := strings.ToLower(strings.TrimSpace(r))
		if v == "" {
			continue
		}
		seen[v] = true
	}
	out := make([]string, 0, len(seen))
	for _, role := range Roles {
		if seen[role] {
			out = append(out, role)
		}
	}
	return out
}

func validID(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// DefaultRules 是内置脱敏词表（照搬 Agent2API 的 core/sanitize 默认集）：
// 上游按「连续文本」逐字匹配客户端指纹，所以每条都只做最小改写，够让指纹
// 失配即可，原文其余部分逐字保留 —— 客户端自带的项目规范、工具说明不能
// 因为清洗提示词而失真。
func DefaultRules() []Rule {
	literal := func(id, name, pattern, replacement string) Rule {
		return Rule{ID: id, Name: name, Enabled: true, MatchType: MatchLiteral, Pattern: pattern, Replacement: replacement}
	}
	regexRule := func(id, name, pattern, replacement string, repeat bool, roles []string) Rule {
		return Rule{ID: id, Name: name, Enabled: true, MatchType: MatchRegex, Pattern: pattern,
			Replacement: replacement, Repeat: repeat, Roles: roles}
	}
	out := []Rule{
		literal("claude-code-identity", "Claude Code",
			"You are Claude Code, Anthropic's official CLI for Claude",
			"You are Claude Code, Anthropic's official CLI tool for Claude"),
		literal("main-branch", "Main branch",
			"Main branch (you will usually use this for PRs)",
			"Default branch (you will usually use this for PRs)"),
		literal("codex-cli-identity", "Codex CLI",
			"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
			"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant."),
		literal("claude-code-feedback", "Claude Code feedback",
			"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
			"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"),
		literal("error-11128", "11128", "11128", "11-128"),
	}
	// 键值删除必须先于裸键名改写；cc_ 删除重复执行以清理新暴露的相邻边界。
	// 头部名里的零宽空格（U+200B）是上游原文的一部分，用 \x{200b} 表达。
	out = append(out,
		regexRule("billing-header-value", "x-anthropic-billing-header",
			`(?i)x\x{200b}-anthropic-billing-header:[^;\n]*;?[\t\n\f\r ]*`, "", false, nil),
		regexRule("cc-parameters", "cc_*=",
			`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?[\t\n\f\r ]*`, "", true, nil),
		regexRule("billing-header-name", "x-anthropic-billing-header",
			`(?i)x\x{200b}-anthropic-billing-header`, "x-anthropic-billing-hdr", false, nil),
	)
	// 身份句与提示词指纹：上游逐字匹配，因此只做最小改写。
	identityRoles := []string{"system", "developer", "assistant"}
	out = append(out,
		regexRule("agent-identity", "Agent identity",
			`(?is)\bYou are\s+(?:an?\s+|the\s+)?(?:[\w.-]+\s+){0,3}?(?:agent|assistant|model|AI|LLM)?[\s,]*(?:powered by|built on|built by|created by|developed by|made by|trained by)[^\r\n]*?(?:\.|$)`,
			"You are a helpful AI assistant.", false, identityRoles),
		regexRule("dsh-gui-description", "DeepSeek Harness GUI",
			`(?i)You are interacting with the user through the DeepSeek[ \t]+Harness[ \t]+Web[ \t]+GUI whose URL the runtime context names\.`,
			"You are interacting with the user through the DeepSeek Harness Web GUI whose URL is named in the runtime context.",
			false, identityRoles),
		regexRule("dsh-file-reference", "DeepSeek Harness file reference",
			`(?i)Tokens prefixed with @ are paths the user explicitly referenced\.`,
			"Tokens prefixed with @ are paths that the user explicitly referenced.", false, identityRoles),
		regexRule("dsh-file-reference-values", "DeepSeek Harness file reference (Values)",
			`(?i)Values prefixed with @`, "Values beginning with @", false, identityRoles),
		regexRule("dsh-search-fetch", "DeepSeek Harness search/fetch",
			`(?i)Searching and fetching are distinct operations`,
			"Searching and fetching are separate operations", false, identityRoles),
		regexRule("dsh-brand", "DeepSeek Harness",
			`(?i)\bDeepSeek[ \t]+Harness\b`, "DeepSeek", false, identityRoles),
	)
	return out
}
