package sensenova

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/relay"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// Upstream 把日日新能力包成 relay.SNUpstream / creditsync.SNBalance。
// relay 不 import 本包（它只认自己定义的接口），本包可以安全地 import relay。
type Upstream struct {
	C  *Client
	ST *store.Store
}

// refreshTimeout 是转发热路径上读额度的自设上限：额度查询是控制面请求，
// 不能因为上游控制台慢把一条对话请求卡在「冷却前」几十秒。
const refreshTimeout = 8 * time.Second

func NewUpstream(c *Client, st *store.Store) *Upstream { return &Upstream{C: c, ST: st} }

// Upstream 必须满足 relay 的日日新接口，编译期校验。
var _ relay.SNUpstream = (*Upstream)(nil)
var _ interface {
	FetchUsageFor(ctx context.Context, acc *store.Account) (float64, string, error)
} = (*Upstream)(nil)

// EnsureToken 保证账号控制台登录票可用：
//
//  1. 库里的登录票没过期 → 直接用；
//  2. 有 refresh_token → 续期；
//  3. 有账号密码 → 重登；
//  4. 都不行 → 报「凭证失效」（由转发层按 relogin 处理）。
//
// 返回的凭据供调用方写回；本方法自身不落库，保持无副作用，
// 便于转发层决定「成功后才写回」。
func (u *Upstream) EnsureToken(ctx context.Context, acc *store.Account) (*relay.SNCredential, error) {
	if acc == nil || !acc.IsSenseNova() {
		return nil, errors.New("不是日日新账号")
	}
	now := time.Now()
	if strings.TrimSpace(acc.SNToken) != "" && acc.SNTokenExp > now.UnixMilli() {
		return &relay.SNCredential{ConsoleToken: acc.SNToken, ExpiresAt: acc.SNTokenExp}, nil
	}
	if TokenReusable(acc.SNToken, now) {
		info := ParseToken(acc.SNToken)
		exp := int64(0)
		if info.Exp > 0 {
			exp = info.Exp * 1000
		}
		// 补写过期时刻，避免每次都用 60 秒窗口重算。
		return &relay.SNCredential{ConsoleToken: acc.SNToken, ExpiresAt: exp}, nil
	}

	if strings.TrimSpace(acc.SNRefreshToken) != "" {
		res, err := u.C.Refresh(ctx, acc.SNRefreshToken)
		if err == nil {
			return credentialOf(res), nil
		}
		// 续期失败不在此直接判死：账号密码还在时继续走重登
	}

	if strings.TrimSpace(acc.SNUsername) != "" && acc.SNPassword != "" {
		res, err := u.C.Login(ctx, acc.SNUsername, acc.SNPassword)
		if err != nil {
			return nil, err
		}
		return credentialOf(res), nil
	}
	return nil, newError("token", "控制台登录票已过期，且没有 refresh_token / 账号密码可用", "请补齐账号密码或重新导入")
}

func credentialOf(res *LoginResult) *relay.SNCredential {
	return &relay.SNCredential{ConsoleToken: res.AccessToken, ExpiresAt: res.ExpiresAt, Username: res.Username}
}

// FetchUsageFor 确保登录票可用、写入库（若续期/重登过），再读积分池。
// 返回主池剩余量（用于 credits）与完整池的存储 JSON（用于面板）。
func (u *Upstream) FetchUsageFor(ctx context.Context, acc *store.Account) (float64, string, error) {
	cred, err := u.EnsureToken(ctx, acc)
	if err != nil {
		return 0, "", err
	}
	if cred != nil && cred.ConsoleToken != "" && cred.ConsoleToken != acc.SNToken {
		exp := cred.ExpiresAt
		patch := store.AccountPatch{SNToken: &cred.ConsoleToken, SNTokenExp: &exp}
		if cred.Username != "" {
			patch.SNUsername = &cred.Username
		}
		if err := u.ST.UpdateAccount(ctx, acc.ID, patch); err != nil {
			return 0, "", err
		}
		acc.SNToken, acc.SNTokenExp = cred.ConsoleToken, exp
	}
	info := ParseToken(acc.SNToken)
	usage, err := u.C.FetchUsage(ctx, acc.SNToken, info.TenantID)
	if err != nil {
		return 0, "", err
	}
	remaining, _ := usage.Total()
	return remaining, usage.MarshalStored(), nil
}

// ProvisionKey 用控制台登录票创建 API Key。调用方应先 EnsureToken 并写回。
func (u *Upstream) ProvisionKey(ctx context.Context, acc *store.Account) (string, error) {
	if acc == nil {
		return "", errors.New("账号为空")
	}
	token := acc.SNToken
	if strings.TrimSpace(token) == "" {
		return "", newError("api-key", "没有控制台登录票，无法创建 API Key", "")
	}
	name := "raccoon-relay-" + acc.ProviderName()
	key, err := u.C.CreateAPIKey(ctx, token, name)
	if err != nil {
		return "", err
	}
	return key, nil
}

// ModelsFor 实现 relay.SNModelSource：用该账号的推理 Key 拉实时模型清单。
// 账号没有 Key 时先补建一枚；任何一步失败都返回 nil，让调用方回落内置快照。
func (u *Upstream) ModelsFor(ctx context.Context, acc *store.Account) []raccoon.Model {
	if acc == nil || !acc.IsSenseNova() {
		return nil
	}
	key := strings.TrimSpace(acc.Token)
	if key == "" {
		k, err := u.ProvisionKey(ctx, acc)
		if err != nil {
			return nil
		}
		key = k
	}
	live := u.C.LiveModels(ctx, key)
	if len(live) == 0 {
		return nil
	}
	out := make([]raccoon.Model, 0, len(live))
	for _, m := range live {
		out = append(out, raccoon.Model{
			ID:              m.ID,
			Name:            m.Name,
			ContextLength:   m.ContextLength,
			MaxOutputTokens: m.MaxOutput,
			SupportsImages:  m.SupportsImage,
			SupportsTools:   m.SupportsTools,
			SupportsReason:  m.SupportsReason,
		})
	}
	return out
}

// OpenChat 实现 relay.SNUpstream。
func (u *Upstream) OpenChat(ctx context.Context, apiKey string, body map[string]any) (*raccoon.ChatStream, error) {
	return u.C.OpenChat(ctx, apiKey, body)
}

// NextReset 实现 relay.SNQuotaSource：返回该账号额度恢复的绝对毫秒时刻。
//
// 日日新没有每日签到，额度按 5 小时 / 本周窗口滚动，所以「停到下一次签到补分」
// 对它没有意义。这里直接问控制台要 reset_at，并顺手刷新一次积分：
//
//   - 短超时（refreshTimeout）：转发热路径上不能为了等一个额度查询把请求拖住；
//   - 只有「确实读到了用尽窗口」才通过 staleCredits 回写余额，读不到不改旧值；
//   - 返回 ok=false 时调用方回退到默认恢复时刻，而不是把号放出来。
func (u *Upstream) NextReset(ctx context.Context, acc *store.Account) (int64, bool, int64, bool) {
	if acc == nil || !acc.IsSenseNova() {
		return 0, false, 0, false
	}
	qctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	usage, err := u.FetchUsageForUsed(qctx, acc)
	if err != nil || usage == nil {
		return 0, false, 0, false
	}
	until := usage.NextReset()
	if until <= time.Now().UnixMilli() {
		// 上游给的重置时刻已经过去（或根本没给）：按窗口常识兜底会误导，
		// 交给调用方回退默认时刻，由余额巡检决定何时真正放出来。
		until = 0
	}
	remaining, _ := usage.Total()
	credits := int64(remaining)
	stale := usage.PoolsExhausted()
	return until, until > 0, credits, stale
}

// FetchUsageForUsed 与 FetchUsageFor 相同，但额外返回归一化后的 Usage，
// 供额度耗尽时判断「哪一格用尽、何时重置」。登录票续期与回写逻辑复用。
func (u *Upstream) FetchUsageForUsed(ctx context.Context, acc *store.Account) (*Usage, error) {
	if acc == nil || !acc.IsSenseNova() {
		return nil, errors.New("不是日日新账号")
	}
	cred, err := u.EnsureToken(ctx, acc)
	if err != nil {
		return nil, err
	}
	if cred != nil && cred.ConsoleToken != "" && cred.ConsoleToken != acc.SNToken {
		exp := cred.ExpiresAt
		patch := store.AccountPatch{SNToken: &cred.ConsoleToken, SNTokenExp: &exp}
		if cred.Username != "" {
			patch.SNUsername = &cred.Username
		}
		if err := u.ST.UpdateAccount(ctx, acc.ID, patch); err != nil {
			return nil, err
		}
		acc.SNToken, acc.SNTokenExp = cred.ConsoleToken, exp
	}
	info := ParseToken(acc.SNToken)
	return u.C.FetchUsage(ctx, acc.SNToken, info.TenantID)
}
