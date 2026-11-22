package autoclaw

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/relay"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// Upstream 把 AutoClaw 能力包成 relay.ExtUpstream，并额外提供额度与签到。
type Upstream struct {
	C  *Client
	ST *store.Store
	// Refresher 在续期前后拿跨实例锁；nil 时只用进程内锁。
	Refresher interface {
		LockAccount(ctx context.Context, id string) (release func(), ok bool)
	}
}

func NewUpstream(c *Client, st *store.Store) *Upstream { return &Upstream{C: c, ST: st} }

var _ relay.ExtUpstream = (*Upstream)(nil)

// EnsureCredential 保证凭据可用：临期则续期并整体写回。
//
// 续期是这类渠道最脆弱的一环：refresh_token 一次性，多副本同时续期时
// 后写的那个会把轮换结果覆盖成作废的旧值。所以这里三道闸：
//  1. 进程内按账号单飞（LockForAccount）；
//  2. 写回用库里的 ext_rev 做 CAS（ReplaceExtCredential），版本被人改过就放弃；
//  3. CAS 失败时重读库，用对方写回的新凭据，而不是硬写自己的。
func (u *Upstream) EnsureCredential(ctx context.Context, acc *store.Account) error {
	cred, err := DecodeCred(acc)
	if err != nil {
		return &raccoon.UpstreamError{Status: 401, Auth: true, Message: err.Error()}
	}
	if cred.Access != "" && cred.Expires > time.Now().Add(2*time.Minute).UnixMilli() {
		return nil
	}
	if cred.Refresh == "" {
		if cred.Access != "" {
			return nil // 只有 access、没到期就不用管
		}
		return &raccoon.UpstreamError{Status: 401, Auth: true, Message: "AutoClaw 凭据已过期且没有 refresh_token，请重新导入"}
	}

	mu := LockForAccount(acc.ID)
	mu.Lock()
	defer mu.Unlock()

	// 双检：等锁期间别的请求可能已经续期写回。
	//
	// 拿到的新凭据必须「有到期时刻且未临期」才算别人刷好了。Expires == 0
	// 表示到期未知（例如手工导入只给了 token），不能当成「永不过期」——
	// 否则一次续期就会把自己判成成功，实际手里还是旧凭据。
	if fresh, gerr := u.ST.GetAccount(ctx, acc.ID); gerr == nil && fresh != nil {
		if fc, derr := DecodeCred(fresh); derr == nil && fc.Access != "" {
			if fc.Expires > time.Now().Add(2*time.Minute).UnixMilli() {
				acc.ExtToken, acc.ExtTokenExp, acc.ExtRefreshToken = fresh.ExtToken, fresh.ExtTokenExp, fresh.ExtRefreshToken
				acc.ExtExtra, acc.ExtRev = fresh.ExtExtra, fresh.ExtRev
				return nil
			}
			cred, acc = fc, fresh
		}
	}

	next, err := u.C.Refresh(ctx, cred, acc.Station())
	if err != nil {
		return err
	}
	ok, werr := u.ST.ReplaceExtCredential(ctx, acc.ID, acc.ExtRev, store.ExtCredential{
		Token:        next.Access,
		TokenExp:     next.Expires,
		RefreshToken: next.Refresh,
		// 续期成功本身就是「refresh 现在还活着」的证据；把它记成到期时刻的
		// 下界，下一次就不会因为 Expires == 0 被重复当作临期再刷一次。
		RefreshExp: refreshActiveUntil(),
		Site:       acc.Station(),
		Extra:      mustJSON(next),
	})
	if werr != nil {
		return werr
	}
	if !ok {
		// 别的副本先写了一版，用它写回的结果。
		if fresh, gerr := u.ST.GetAccount(ctx, acc.ID); gerr == nil && fresh != nil {
			acc.ExtToken, acc.ExtTokenExp, acc.ExtRefreshToken = fresh.ExtToken, fresh.ExtTokenExp, fresh.ExtRefreshToken
			acc.ExtExtra, acc.ExtRev = fresh.ExtExtra, fresh.ExtRev
		}
		return nil
	}
	acc.ExtToken, acc.ExtTokenExp, acc.ExtRefreshToken, acc.ExtExtra = next.Access, next.Expires, next.Refresh, mustJSON(next)
	acc.ExtRev++
	return nil
}

func mustJSON(c Credential) string {
	raw, _ := json.Marshal(c)
	return string(raw)
}

// refreshActiveUntil 是一次成功续期后「refresh_token 仍然活着」的保守下界。
// 上游没告诉我们 refresh_token 的有效期，取较短的一段：到期后如果还需要
// 续期，会重新尝试一次刷新，失败再按终态处理。
func refreshActiveUntil() int64 {
	return time.Now().Add(6 * time.Hour).UnixMilli()
}

// OpenChat 实现 relay.ExtUpstream。
func (u *Upstream) OpenChat(ctx context.Context, acc *store.Account, body map[string]any) (*raccoon.ChatStream, error) {
	// 模型名在这一层翻译成上游 routeModelId。body 是每条请求独立的副本，
	// 直接改不会影响别的账号的重试。
	model, _ := body["model"].(string)
	if model != "" {
		body = cloneBody(body)
		catalog := FallbackModels(acc.Station())
		body["model"] = ResolveRoute(model, catalog)
	}
	return u.C.OpenChat(ctx, acc, body)
}

func cloneBody(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Models 实现 relay.ExtUpstream：实时清单优先，拉不到回落内置快照。
func (u *Upstream) Models(ctx context.Context, acc *store.Account) []raccoon.Model {
	if live := u.C.FetchModels(ctx, acc); len(live) > 0 {
		return ToRaccoon(live)
	}
	return ToRaccoon(FallbackModels(acc.Station()))
}

// RefreshQuota 拉余额并写回账号。成功返回最新余额。
//
// 响应异常（字段缺失）时不覆盖旧值：把「读不到」当成 0 会让号在面板上
// 凭空变成「积分耗尽」。
func (u *Upstream) RefreshQuota(ctx context.Context, acc *store.Account) (int64, error) {
	if err := u.EnsureCredential(ctx, acc); err != nil {
		return 0, err
	}
	q, err := u.C.FetchQuota(ctx, acc)
	var qe *QuotaError
	if errors.As(err, &qe) || (err == nil && q.Total < 0) {
		return acc.Credits, err
	}
	if err != nil {
		return 0, err
	}
	credits := int64(q.Total)
	at := time.Now().UnixMilli()
	patch := store.AccountPatch{
		Credits: &credits, CreditsAt: &at,
		SNPool: strPtr(q.MarshalStored()), SNPoolAt: &at,
	}
	if err := u.ST.UpdateAccount(ctx, acc.ID, patch); err != nil {
		return credits, err
	}
	acc.Credits, acc.CreditsAt = credits, at
	return credits, nil
}

// StoredQuota 返回上次落库的额度快照，供面板展示。
func StoredQuota(acc *store.Account) *Quota {
	return UnmarshalQuota(acc.SNPool)
}

// Checkin 实现调度器的签到需求。
func (u *Upstream) Checkin(ctx context.Context, acc *store.Account) (bool, bool, int64, string) {
	if err := u.EnsureCredential(ctx, acc); err != nil {
		return false, false, 0, err.Error()
	}
	res := u.C.Checkin(ctx, acc)
	return res.OK, res.Already, res.Reward, res.Message
}
