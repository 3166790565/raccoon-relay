package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/3166790565/raccoon-relay/internal/pool"
	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// Engine 把「选号 → 刷新 → 上游请求 → 失败换号」串起来，三条协议共用。
type Engine struct {
	ST           *store.Store
	Pool         *pool.Pool
	Cool         *pool.CooldownBook
	Client       *raccoon.Client
	Refresher    *Refresher
	DefaultModel string

	// 重试策略可在运行期从面板修改，用锁保护。
	mu                  sync.RWMutex
	maxRetry            int
	retryUntilExhausted bool
}

// RetryPolicy 返回当前重试策略。
func (e *Engine) RetryPolicy() (untilExhausted bool, maxRetry int) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.retryUntilExhausted, e.maxRetry
}

// SetRetryPolicy 运行期更新重试策略。
// untilExhausted 为 true 时忽略 maxRetry，一直换号到号池无可用账号为止。
func (e *Engine) SetRetryPolicy(untilExhausted bool, maxRetry int) {
	if maxRetry < 0 {
		maxRetry = 0
	}
	e.mu.Lock()
	e.retryUntilExhausted, e.maxRetry = untilExhausted, maxRetry
	e.mu.Unlock()
}

// Attempt 是一次换号尝试的上下文，供各协议实现写入增量。
type Attempt struct {
	Account *store.Account
	Lease   *pool.Lease
	Token   string
	IDN     raccoon.Identity
	// Started 标记是否已经向客户端写过内容；写过就不能再换号。
	Started bool
}

// RunOpenAI 执行 OpenAI 协议的一次完整转发（含换号重试）。
// 返回的 usage 与 account 用于写流水。
func (e *Engine) RunOpenAI(ctx context.Context, req *ChatContext, sink OpenAISink) (Result, error) {
	var excluded = map[string]bool{}
	var lastErr error

	for attemptNo := 0; ; attemptNo++ {
		untilExhausted, maxRetry := e.RetryPolicy()
		if !untilExhausted && attemptNo > maxRetry {
			break
		}
		lease, err := e.Pool.Pick(ctx, req.Model, excluded)
		if err != nil {
			return Result{}, err
		}
		if lease == nil {
			break
		}
		acc := lease.Account
		excluded[acc.ID] = true

		at := &Attempt{Account: acc, Lease: lease, IDN: identityOf(acc)}
		leaseErr := e.runOne(ctx, at, req, sink)
		e.Pool.ReleaseLease(lease)

		if leaseErr == nil {
			return Result{AccountID: acc.ID, Model: req.Model}, nil
		}
		lastErr = leaseErr

		kind, switchable, _, _ := classify(leaseErr)
		// 429 只罚「这个号 × 这个模型」，不把整个号停掉：同一个号换个模型往往还是好的。
		// 只有模型级冷却被关闭时才退化为整号短冷却。
		if isModelScoped(leaseErr) || (kind == pool.FailRate && e.Cool.ModelEnabled()) {
			e.Cool.CoolModel(ctx, acc.ID, req.Model, leaseErr.Error())
			if at.Started {
				return Result{AccountID: acc.ID, Model: req.Model}, leaseErr
			}
			continue
		}
		e.Cool.Cool(ctx, acc.ID, kind, leaseErr.Error())
		if at.Started {
			// 已经写出过内容，不能重试，只能如实报错
			return Result{AccountID: acc.ID, Model: req.Model}, leaseErr
		}
		if !switchable {
			return Result{AccountID: acc.ID, Model: req.Model}, leaseErr
		}
	}

	if lastErr == nil {
		lastErr = errNoAccount
	}
	return Result{}, lastErr
}

var errNoAccount = errors.New("号池没有可用账号（全部冷却、积分耗尽或凭证失效）")

// runOne 是对单个账号的一次尝试。
func (e *Engine) runOne(ctx context.Context, at *Attempt, req *ChatContext, sink OpenAISink) error {
	token, err := e.Refresher.EnsureFresh(ctx, at.Account)
	if err != nil {
		return err
	}
	at.Token = token

	body := req.Body
	idn := at.IDN
	chatReq := raccoon.ChatRequest{
		Body:    body,
		Token:   token,
		IDN:     idn,
		UID:     at.Account.UID,
		Title:   req.Title,
		Session: raccoon.SessionID(at.Account.UID, req.SessionSeed),
	}
	stream, err := e.Client.OpenChat(ctx, chatReq)
	if err != nil && shouldRefreshRetry(err, at.Account.TokenExp) {
		// EnsureFresh 只在临期窗口内刷新。本地 exp 字段缺失或与上游有偏差时，
		// 手里这枚 access 可能已经被上游判过期——先强刷一次再定论，
		// 否则一次 401 就把还有效的 refresh_token 连同账号一起打成「需重登」。
		fresh, rerr := e.Refresher.RefreshNow(ctx, at.Account)
		if rerr != nil {
			var ue *raccoon.UpstreamError
			if errors.As(rerr, &ue) && ue.Auth {
				return rerr // refresh_token 真的没了，这个号确实要重登
			}
			// 刷新接口自己抽风（网络/5xx）：这是临时故障，不能算到账号头上。
			// 用 Status 0 走 FailServer → 短冷却 + 换号，账号不会被打成 relogin。
			return &raccoon.UpstreamError{Status: 0, Message: "刷新令牌失败（临时）：" + rerr.Error()}
		}
		at.Token = fresh
		chatReq.Token = fresh
		stream, err = e.Client.OpenChat(ctx, chatReq)
	}
	if err != nil {
		return err
	}
	defer stream.Close()

	// 从上游拿到 200 之后才回写状态，避免把「连接失败」误记为在线
	if err := sink.Begin(); err != nil {
		return err
	}
	at.Started = true

	for {
		ev, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if ev.Delta != nil {
			if err := sink.Delta(*ev.Delta); err != nil {
				return err
			}
		}
		if ev.Usage != nil {
			sink.Usage(*ev.Usage)
		}
		if ev.FinishReason != "" {
			sink.Finish(ev.FinishReason)
		}
	}

	if !stream.Seen() {
		return &raccoon.UpstreamError{Status: 502, Message: "小浣熊上游返回空响应"}
	}
	e.Cool.MarkOnline(ctx, at.Account.ID)
	// 成功即清零这条「账号×模型」的失败链，下次再 429 从 3 分钟重新计。
	e.Cool.ClearModel(ctx, at.Account.ID, req.Model)
	sink.Complete()
	return nil
}

// ChatContext 是一次请求的输入。
type ChatContext struct {
	Model       string
	Body        map[string]any
	Title       string
	SessionSeed string
}

// Result 是转发结果的元信息。
type Result struct {
	AccountID string
	Model     string
}

// OpenAISink 是 OpenAI 流的写出接口，由具体协议实现提供。
type OpenAISink interface {
	Begin() error
	Delta(raccoon.Delta) error
	Usage(raccoon.Usage)
	Finish(reason string)
	Complete()
}

// BuildChatContext 从原始请求体构造转发上下文。
func BuildChatContext(model string, body map[string]any, defaultModel string) *ChatContext {
	prepared := raccoon.PrepareBody(model, body, defaultModel)
	messages, _ := prepared["messages"].([]any)
	title := ""
	if len(messages) > 0 {
		title = firstUserText(messages)
	}
	seed, _ := json.Marshal(messages)
	return &ChatContext{
		Model:       prepared["model"].(string),
		Body:        prepared,
		Title:       title,
		SessionSeed: string(seed),
	}
}

func firstUserText(messages []any) string {
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok || msg["role"] != "user" {
			continue
		}
		if s, ok := msg["content"].(string); ok {
			return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
		}
		if arr, ok := msg["content"].([]any); ok {
			for _, part := range arr {
				p, ok := part.(map[string]any)
				if !ok {
					continue
				}
				if t, ok := p["text"].(string); ok && t != "" {
					return strings.TrimSpace(strings.Join(strings.Fields(t), " "))
				}
			}
		}
	}
	return ""
}
