package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/3166790565/raccoon-relay/internal/cache"
	"github.com/3166790565/raccoon-relay/internal/config"
	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// Catalog 缓存上游模型目录，避免每次 /v1/models 都打上游。
type Catalog struct {
	ST      *store.Store
	Client  *raccoon.Client
	Default string
	TTL     time.Duration
	// SN 是日日新实时模型清单的来源；nil 时只用内置快照。
	SN SNModelSource

	mu     sync.Mutex
	models []raccoon.Model
	at     time.Time
}

func NewCatalog(st *store.Store, c *raccoon.Client, def string) *Catalog {
	return &Catalog{ST: st, Client: c, Default: def, TTL: 10 * time.Minute}
}

// List 返回模型清单。优先用缓存；拉取失败回落内置默认模型。
func (c *Catalog) List(ctx context.Context) []raccoon.Model {
	c.mu.Lock()
	if len(c.models) > 0 && time.Since(c.at) < c.TTL {
		out := c.models
		c.mu.Unlock()
		return out
	}
	c.mu.Unlock()

	models := c.fetch(ctx)
	if len(models) == 0 {
		c.mu.Lock()
		if len(c.models) > 0 {
			out := c.models
			c.mu.Unlock()
			return out
		}
		c.mu.Unlock()
		return []raccoon.Model{{ID: c.Default, Name: c.Default, ContextLength: 180000, MaxOutputTokens: 80000}}
	}
	c.mu.Lock()
	c.models, c.at = models, time.Now()
	c.mu.Unlock()
	return models
}

func (c *Catalog) fetch(ctx context.Context) []raccoon.Model {
	accounts, err := c.ST.ListAccounts(ctx)
	if err != nil {
		return nil
	}
	// 两台供应商的模型并集：小浣熊走它的目录，日日新用内置快照 + 实时清单。
	var raccoonModels, snModels []raccoon.Model
	for _, a := range accounts {
		if !a.Enabled || a.Status != store.StatusOnline {
			continue
		}
		if a.IsSenseNova() {
			// 实时清单优先；拉不到（账号没 Key、上游故障）再回落内置快照。
			if len(snModels) == 0 && c.SN != nil {
				if live := c.SN.ModelsFor(ctx, a); len(live) > 0 {
					snModels = live
					// 顺手记给路由层：上游新上线的模型名也能走对上游。
					ids := make([]string, 0, len(live))
					for _, m := range live {
						ids = append(ids, m.ID)
					}
					liveSNModels.replace(ids)
				}
			}
			if len(snModels) == 0 {
				snModels = senseNovaModels()
			}
			continue
		}
		if a.Token == "" || len(raccoonModels) > 0 {
			continue
		}
		seen, err := c.fetchRaccoonCatalog(ctx, a)
		if err != nil || len(seen) == 0 {
			continue // 这个号拉不动就换下一个，不能让一个坏号把整个目录清空
		}
		raccoonModels = seen
	}
	if len(snModels) == 0 {
		// 没有任何日日新账号时，若库里只有日日新模型可选（例如只导入了账号
		// 但被停用），也让 /v1/models 至少能看到它们，方便客户端先配置。
		if hasAnySenseNovaSeat(accounts) {
			snModels = senseNovaModels()
		}
	}
	return append(raccoonModels, snModels...)
}

// fetchRaccoonCatalog 拉小浣熊目录，并合并多个账号的可见项。
//
// 品类（模型清单）按账号订阅不同而不同：只看第一个在线号的目录，
// 会把别的号能用的模型整个漏掉。这里最多试 maxCatalogAccounts 个号，
// 把各自的模型按 id 去重合并；第一个号的顺序优先（目录本身是有序的）。
func (c *Catalog) fetchRaccoonCatalog(ctx context.Context, first *store.Account) ([]raccoon.Model, error) {
	const maxCatalogAccounts = 3
	merged, seen := make([]raccoon.Model, 0, 32), map[string]bool{}
	lastErr := error(nil)
	candidates := []*store.Account{first}
	accounts, err := c.ST.ListAccounts(ctx)
	if err == nil {
		for _, a := range accounts {
			if a.ID != first.ID && !a.IsSenseNova() && a.Enabled && a.Status == store.StatusOnline && a.Token != "" {
				candidates = append(candidates, a)
				if len(candidates) >= maxCatalogAccounts {
					break
				}
			}
		}
	}
	for _, a := range candidates {
		models, err := c.Client.Catalog(ctx, a.Token, identityOf(a))
		if err != nil {
			lastErr = err
			continue
		}
		for _, m := range models {
			if m.ID == "" || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			merged = append(merged, m)
		}
	}
	if len(merged) == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("没有可用的目录来源")
		}
		return nil, lastErr
	}
	return merged, nil
}

func hasAnySenseNovaSeat(accounts []*store.Account) bool {
	for _, a := range accounts {
		if a.IsSenseNova() {
			return true
		}
	}
	return false
}

// senseNovaModels 返回日日新模型清单（内置快照，保证离线也有）。
func senseNovaModels() []raccoon.Model {
	out := make([]raccoon.Model, len(sensenovaModels))
	copy(out, sensenovaModels)
	return out
}

// Server 是数据面 HTTP 处理器。
type Server struct {
	cfg     *config.Config
	st      *store.Store
	engine  *Engine
	catalog *Catalog
	// sem 是跨实例并发闸门；nil 表示不限制并发。
	sem cache.Semaphore
	// Rules 是模型管理规则；nil 表示未启用（不做映射改写）。
	Rules ModelRules
}

func NewServer(cfg *config.Config, st *store.Store, engine *Engine, catalog *Catalog) *Server {
	return &Server{cfg: cfg, st: st, engine: engine, catalog: catalog}
}

// SetModelRules 注入模型管理规则（装配层调用）。
func (s *Server) SetModelRules(r ModelRules) { s.Rules = r }

// buildChatContext 按当前模型规则构造转发上下文。
//
// 被停用的模型直接报错，不进入转发：只从 /v1/models 里藏起来是不够的，
// 客户端手里还留着名字，照样能发请求 —— 那等于「停用」没生效。
func (s *Server) buildChatContext(model string, body map[string]any, sessionKey string) (*ChatContext, error) {
	if s.Rules != nil && model != "" {
		// 别名本身被停用，或者它指向的上游模型被停用，都要拒绝。
		// 只检查别名会漏掉「别名还开着、目标已经下线」这种情况。
		target := model
		if resolved, _, ok := s.Rules.Resolve(model); ok && resolved != "" {
			target = resolved
		}
		if s.Rules.Hidden(model) || (target != model && s.Rules.Hidden(target)) {
			return nil, &ModelDisabledError{Model: target}
		}
	}
	return BuildChatContextWithRules(s.Rules, model, body, DefaultModel, sessionKey), nil
}

// auth 从 Authorization 或 x-api-key 取 Key 并校验。
func (s *Server) auth(r *http.Request) (string, bool) {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(raw), "bearer ") {
		raw = strings.TrimSpace(raw[7:])
	}
	if raw == "" {
		raw = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	if raw == "" {
		return "", false
	}
	return raw, s.st.VerifyKey(r.Context(), raw)
}

func decodeBody(r *http.Request, maxMB int) (map[string]any, error) {
	limit := int64(maxMB) << 20
	if limit <= 0 {
		limit = 32 << 20
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errBodyTooLarge
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, errBadJSON
	}
	return m, nil
}

func (s *Server) logUsage(ctx context.Context, res Result, err error, start time.Time, api string, usage *raccoon.Usage) {
	u := store.UsageLog{
		AccountID: res.AccountID, Model: res.Model, API: api,
		Status: http.StatusOK, MS: time.Since(start).Milliseconds(),
	}
	if err != nil {
		code, msg := publicError(err)
		u.Status = code
		// 流水是事后排查用的，存完整错误；对外响应仍用 publicError 的短文案。
		// 长串在列表里由前端折叠显示，详情弹窗能看到全文。
		u.Error = fullErrorMessage(err, msg)
	}
	if usage != nil {
		u.PromptTokens, u.CompletionTokens, u.TotalTokens = usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens
	}
	// 异步写：每个请求同步 INSERT 在单连接 SQLite 上会排队等写锁。
	s.st.AddUsageAsync(u)
}

// fullErrorMessage 返回写入流水的完整错误文本。
//
// 对外响应必须短且干净（客户端会展示给终端用户），但流水是排查记录：
// 上游返回的 JSON 里往往带 request_id、param 等字段，截断后就查不了。
// 这里保留上游原文，只做长度上限保护。
func fullErrorMessage(err error, fallback string) string {
	var ue *raccoon.UpstreamError
	if errors.As(err, &ue) {
		// Raw 是上游原始响应体，信息最全（含 request_id、param 等）。
		// Message 可能被上游自己截过，只在 Raw 为空时兜底。
		if raw := strings.TrimSpace(ue.Raw); raw != "" {
			return truncateRunes(raw, 4000)
		}
		if msg := strings.TrimSpace(ue.Message); msg != "" {
			return truncateRunes(msg, 4000)
		}
	}
	if msg := strings.TrimSpace(err.Error()); msg != "" {
		return truncateRunes(msg, 4000)
	}
	return fallback
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// Route 是数据面的顶层分发。
func (s *Server) Route(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/health":
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost:
		s.HandleOpenAIChat(w, r)
	case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
		s.HandleModels(w, r)
	case r.URL.Path == "/v1/messages" && r.Method == http.MethodPost:
		s.HandleAnthropicMessages(w, r)
	case r.URL.Path == "/v1/messages/count_tokens" && r.Method == http.MethodPost:
		s.HandleAnthropicCountTokens(w, r)
	case r.URL.Path == "/v1/responses" && r.Method == http.MethodPost:
		s.HandleResponses(w, r)
	case r.Method == http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusNotFound, errorBody(http.StatusNotFound, "未知端点"))
	}
}

var (
	errBodyTooLarge = &simpleErr{"请求体超过上限"}
	errBadJSON      = &simpleErr{"请求体不是合法 JSON"}
)

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }

// BuildEngine 是装配辅助，供 main 调用。
func BuildEngine(cfg *config.Config, st *store.Store, p *pool.Pool, cool *pool.CooldownBook,
	client *raccoon.Client, refresher *Refresher) *Engine {
	e := &Engine{
		ST: st, Pool: p, Cool: cool, Client: client, Refresher: refresher,
		DefaultModel: DefaultModel,
	}
	e.SetRetryPolicy(cfg.RetryUntilExhausted, cfg.MaxRetries)
	return e
}

const DefaultModel = "raccoon-chat-ml-5-5"
