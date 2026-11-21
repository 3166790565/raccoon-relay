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

	// Event 上报运行事件（换号、上游失败、空响应）。nil 时静默跳过。
	// 由装配层注入，避免 relay 直接依赖 admin 的写入路径。
	Event func(store.RuntimeEvent)

	// 重试策略可在运行期从面板修改，用锁保护。
	mu                  sync.RWMutex
	maxRetry            int
	retryUntilExhausted bool
	// retryDelay 是两次换号之间的等待。0 = 不等待（旧行为）。
	// 短暂停顿能打断「同一秒内横扫整个号池」的密集重试形态。
	retryDelay time.Duration
	// affinity 是会话粘性开关与 TTL（同一段对话绑定同一个号）。
	affinity       bool
	affinityTTLVal time.Duration
}

// nowMillis 单独抽出来，便于测试打桩。
func nowMillis() int64 { return time.Now().UnixMilli() }

// 会话粘性的运行期开关，与重试策略共用同一把锁。
// 零值语义：affinity 为 false 时完全不读写绑定表。

// AffinityPolicy 返回会话粘性开关与 TTL。
func (e *Engine) AffinityPolicy() (enabled bool, ttl time.Duration) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.affinity, e.affinityTTLVal
}

// SetAffinityPolicy 运行期更新会话粘性。
func (e *Engine) SetAffinityPolicy(enabled bool, ttl time.Duration) {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	e.mu.Lock()
	e.affinity, e.affinityTTLVal = enabled, ttl
	e.mu.Unlock()
}

func (e *Engine) affinityEnabled() bool {
	on, _ := e.AffinityPolicy()
	return on
}

func (e *Engine) affinityTTL() time.Duration {
	_, ttl := e.AffinityPolicy()
	return ttl
}

// RetryDelay 返回当前换号间隔。
func (e *Engine) RetryDelay() time.Duration {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.retryDelay
}

// SetRetryDelay 运行期更新换号间隔。
func (e *Engine) SetRetryDelay(d time.Duration) {
	if d < 0 {
		d = 0
	}
	e.mu.Lock()
	e.retryDelay = d
	e.mu.Unlock()
}

// report 写一条运行事件，失败静默。
func (e *Engine) report(level, msg, detail string, accountID string) {
	if e.Event == nil {
		return
	}
	e.Event(store.RuntimeEvent{
		Level: level, Scope: store.ScopeRelay, Component: "engine",
		AccountID: accountID, Message: msg, Detail: detail,
	})
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

// committingSink 由「会立刻把字节写给客户端」的 sink 实现（流式 SSE）。
// 聚合型 sink（非流式）的 Begin 只是空实现，上游 200 却不吐帧时换号重试仍然安全：
// 此时客户端一个字节都没收到。没有实现这个接口的 sink 不因 Begin 而锁死重试。
type committingSink interface {
	Commitment() bool
}

// RunOpenAI 执行 OpenAI 协议的一次完整转发（含换号重试）。
// 返回的 usage 与 account 用于写流水。
//
// 一次请求的完整形态：
//  1. 会话粘性：同一段对话优先复用上次成功的号；
//  2. 选号 → 抢在途租约（同号串行）→ 请求上游；
//  3. 成功：解绑失败记录、清退避、刷新绑定；
//  4. 失败：退避本号（短暂跳过，不是停号）、解绑会话、等待 retryDelay 后换号。
func (e *Engine) RunOpenAI(ctx context.Context, req *ChatContext, sink OpenAISink) (Result, error) {
	var excluded = map[string]bool{}
	var lastErr error
	// lastAcc 记住最后一次尝试的账号：全池失败时返回它，流水与面板才不会只显示一个「-」，
	// 事后至少知道「最后卡在哪个号上」。
	var lastAcc string

	// 会话粘性：先把绑定过的号设为优先。绑定失效（号下线/冷却）时
	// 选号层自然跳过它，不会因为这个提示而卡住。
	affinity := e.affinityEnabled()
	if affinity && req.SessionKey != "" {
		if bound, ok := e.ST.BoundAccount(ctx, req.SessionKey, nowMillis()); ok {
			e.Pool.SetPrefer(bound)
		}
	}
	defer e.Pool.SetPrefer("")

	for attemptNo := 0; ; attemptNo++ {
		untilExhausted, maxRetry := e.RetryPolicy()
		if !untilExhausted && attemptNo > maxRetry {
			break
		}
		// 换号前短暂停顿：同一秒内横扫整个号池是最明显的密集重试形态。
		if attemptNo > 0 {
			if d := e.RetryDelay(); d > 0 {
				select {
				case <-ctx.Done():
					return Result{AccountID: lastAcc, Model: req.Model}, errClientGone
				case <-time.After(d):
				}
			}
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
		lastAcc = acc.ID

		// 在途租约：同一账号同时只跑一个请求。抢不到说明刚被别的请求占了，
		// 跳过它去选下一个号，而不是排队等——换号比等待更符合网关的定位。
		if _, ok := e.Pool.AcquireLease(ctx, acc); !ok {
			e.Pool.ReleaseLease(lease)
			e.report(store.LevelInfo, "账号正在被其它请求占用，换号",
				"账号："+acc.Name+"\n模型："+req.Model, acc.ID)
			continue
		}

		at := &Attempt{Account: acc, Lease: lease, IDN: identityOf(acc)}
		leaseErr := e.runOne(ctx, at, req, sink)
		e.Pool.ReleaseAccountLease(ctx, acc)
		e.Pool.ReleaseLease(lease)

		if leaseErr == nil {
			// 成功：清退避与失败链，并把这段会话绑到这个号上。
			e.Pool.ClearBackoff(ctx, acc.ID)
			if affinity && req.SessionKey != "" {
				_ = e.ST.BindSession(ctx, req.SessionKey, acc.ID, req.Model,
					e.affinityTTL().Milliseconds(), nowMillis())
			}
			return Result{AccountID: acc.ID, Model: req.Model}, nil
		}
		lastErr = leaseErr

		// 客户端已经断开：不罚号、不换号。继续重试只会拿别的号再打一次上游，
		// 结果仍然写不出去，还会在号池里制造假故障。
		if isClientGone(leaseErr) {
			e.report(store.LevelWarn, "客户端已断开，本次请求终止",
				fmt.Sprintf("模型：%s\n账号：%s\n尝试序号：%d\n原因：%s",
					req.Model, acc.Name, attemptNo+1, strings.TrimSpace(leaseErr.Error())), acc.ID)
			return Result{AccountID: acc.ID, Model: req.Model}, errClientGone
		}

		kind, switchable, code, publicMsg := classify(leaseErr)
		level := store.LevelWarn
		if kind == pool.FailRelogin || kind == pool.FailRejected || !switchable {
			level = store.LevelError
		}

		// 换号退避：只针对「上游波动」类失败（403 / 5xx / 网络）。
		// 冷却类已经有自己的时长，再叠一层退避没有意义；
		// 内容拒答（FailRejected）是请求本身的问题，换号都不做。
		if switchable && kind == pool.FailTransient {
			if until := e.Pool.MarkBackoff(ctx, acc.ID, publicMsg); until > 0 {
				detail := fmt.Sprintf("模型：%s\n上游返回码：%d\n原因：%s\n退避至：%s",
					req.Model, code, strings.TrimSpace(publicMsg),
					time.UnixMilli(until).Format("2006-01-02 15:04:05"))
				e.report(store.LevelWarn, "上游波动，账号短暂退避",
					detail+"\n处置：该号在退避窗口内不再被选中", acc.ID)
			}
		}
		// 失败即解绑会话：这段对话不该继续粘在一个刚出问题的号上。
		if affinity && req.SessionKey != "" {
			_ = e.ST.UnbindSession(ctx, req.SessionKey)
		}
		detail := fmt.Sprintf("模型：%s\n上游返回码：%d\n尝试序号：%d\n原因：%s",
			req.Model, code, attemptNo+1, strings.TrimSpace(leaseErr.Error()))
		// 把上游的原始现场接在原因后面：403 的真实理由通常只在响应头/响应体里，
		// 面板「错误详情」要能直接看到，否则只剩一句 HTTP 403 没法排查。
		var ue *raccoon.UpstreamError
		if errors.As(leaseErr, &ue) {
			if block := diagnosticBlock(ue); block != "" {
				detail += "\n" + block
			}
		}
		// 200108 这类「上游判定账号异常」处罚的是整个账号：换模型也绕不过去，
		// 所以按账号级阶梯冷却（3 → 5 → 10… 分钟），阶梯到期自动恢复。
		if kind == pool.FailAccountAbnormal {
			if e.Cool.AccountAbnormalEnabled() {
				e.Cool.CoolAccountLadder(ctx, acc.ID, publicMsg)
				e.report(level, "账号级冷却 · "+publicMsg, detail+"\n处置：冷却整个账号，已换号重试", acc.ID)
				if at.Started {
					return Result{AccountID: acc.ID, Model: req.Model}, leaseErr
				}
				continue
			}
			// 开关关闭：不再冷却账号，按上游波动只换号。
			e.report(level, "上游失败："+publicMsg,
				detail+"\n处置：账号异常冷却已关闭，未冷却账号，已换号重试", acc.ID)
			if at.Started {
				return Result{AccountID: acc.ID, Model: req.Model}, leaseErr
			}
			continue
		}
		// 积分耗尽（402 / 上游 1000007）：整号停到下一次签到补分，请求换号继续。
		// 单独处理是为了让恢复时刻对齐该账号的签到计划，而不是写死次日 04:00。
		if kind == pool.FailQuota {
			until := e.Cool.CoolQuota(ctx, acc, publicMsg)
			resume := "未知"
			if until > 0 {
				resume = time.UnixMilli(until).Format("2006-01-02 15:04:05")
			}
			e.report(level, "积分耗尽："+publicMsg,
				detail+"\n处置：冷却账号至下一次签到（"+resume+"），已换号重试", acc.ID)
			if at.Started {
				return Result{AccountID: acc.ID, Model: req.Model}, leaseErr
			}
			continue
		}
		// 429 只罚「这个号 × 这个模型」，不把整个号停掉：同一个号换个模型往往还是好的。
		if isModelScoped(leaseErr) || kind == pool.FailRate {
			if e.Cool.ModelEnabled() {
				e.Cool.CoolModel(ctx, acc.ID, req.Model, leaseErr.Error())
				e.report(level, "模型级冷却："+req.Model+" · "+publicMsg, detail+"\n处置：仅冷却该模型", acc.ID)
				if at.Started {
					return Result{AccountID: acc.ID, Model: req.Model}, leaseErr
				}
				continue
			}
			// 模型级冷却被关闭：429 退化为整号 60 秒短冷却，但 6004/11102 是
			// 「这个号确实用不了这个模型」，不该因此把整个号停掉——
			// 选号层会继续选中它，至少换个模型还能用。
			if isModelScoped(leaseErr) {
				e.report(level, "模型不可用："+req.Model+" · "+publicMsg, detail+"\n处置：不冷却账号，换号重试", acc.ID)
				if at.Started {
					return Result{AccountID: acc.ID, Model: req.Model}, leaseErr
				}
				continue
			}
		}
		e.Cool.Cool(ctx, acc.ID, kind, leaseErr.Error())
		action := "已换号重试"
		if at.Started {
			action = "已向客户端写出内容，无法换号，直接报错"
		} else if !switchable {
			action = "该错误不值得换号，直接报错"
		}
		// 403/5xx/网络波动与 100041/100042 内容拒答都不停号，事件里要如实写「未冷却」，
		// 否则面板上会以为这个号被罚过。
		if pool.NonCooling(kind) && !at.Started {
			if switchable {
				action += "（账号未冷却，保持在线）"
			} else {
				action = "上游拒答请求内容，与账号无关，未冷却"
			}
		}
		e.report(level, "上游失败："+publicMsg, detail+"\n处置："+action, acc.ID)
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
	// 号池抽干才到这里：这是要人处理的状态，按错误级别记。
	e.report(store.LevelError, "号池无可用账号，请求失败",
		"模型："+req.Model+"\n原因："+lastErr.Error(), lastAcc)
	return Result{AccountID: lastAcc, Model: req.Model}, lastErr
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
				e.report(store.LevelError, "凭证失效，refresh_token 已不可用",
					"账号："+at.Account.Name+"\n错误："+rerr.Error()+"\n处置：标记需重登", at.Account.ID)
				return rerr // refresh_token 真的没了，这个号确实要重登
			}
			// 刷新接口自己抽风（网络/5xx）：这是临时故障，不能算到账号头上。
			// 用 Status 0 走 FailServer → 短冷却 + 换号，账号不会被打成 relogin。
			e.report(store.LevelWarn, "刷新令牌失败（临时故障）",
				"账号："+at.Account.Name+"\n错误："+rerr.Error()+"\n处置：按上游故障短冷却换号，不改账号状态",
				at.Account.ID)
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
	// 只有真正往客户端字节流里写过东西，才算「已开始」。流式 sink 的 Begin
	// 会立刻写出首帧，所以马上算已开始；聚合型 sink 的 Begin 是空实现，
	// 此时上游若 200 却一帧不吐，换号重试完全安全。
	if c, ok := sink.(committingSink); ok && c.Commitment() {
		at.Started = true
	}

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
		e.report(store.LevelWarn, "上游返回空响应（502）",
			"账号："+at.Account.Name+"\n模型："+req.Model+"\n上游已建连但没有任何数据块", at.Account.ID)
		return &raccoon.UpstreamError{Status: 502, Message: "小浣熊上游返回空响应"}
	}
	e.Cool.MarkOnline(ctx, at.Account.ID)
	// 成功即清零这两条失败链，下次再出问题从 3 分钟重新计。
	// 账号级链在 MarkOnline 里清零，这里只需要清「账号×模型」那条。
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
	// SessionKey 是会话粘性的键（会话 id / 会话标识），空表示不做粘性。
	// 与 SessionSeed 不同：后者是「内容哈希」（每轮都变），
	// 前者是「这段对话是谁」，多轮之间不变。
	SessionKey string
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
//
// sessionKey 由调用方从请求头/请求体里取（x-session-id / conversation_id 等），
// 用于会话粘性。取不到就是空串，粘性自动关闭，不影响转发。
func BuildChatContext(model string, body map[string]any, defaultModel, sessionKey string) *ChatContext {
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
		SessionKey:  sessionKey,
	}
}

// SessionKeyOf 从请求头或请求体里取会话标识，用于会话粘性。
//
// 顺序与 aigw-backend 对齐：先看三个常见头，再看请求体里的三个字段。
// 取不到就返回空串——粘性是优化，不是转发的前提。
func SessionKeyOf(r *http.Request, body map[string]any) string {
	for _, h := range []string{"X-Session-Id", "X-Conversation-Id", "Session-Id"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return truncateSessionKey(v)
		}
	}
	for _, f := range []string{"session_key", "session_id", "conversation_id"} {
		if v, ok := body[f].(string); ok && strings.TrimSpace(v) != "" {
			return truncateSessionKey(strings.TrimSpace(v))
		}
	}
	return ""
}

// truncateSessionKey 限制长度：粘性键会进库做主键，过长的值没必要保留。
func truncateSessionKey(s string) string {
	const max = 80
	if len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max])
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
