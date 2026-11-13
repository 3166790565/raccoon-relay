// Package creditsync 提供「刷新账号余额」这一件事，供添加账号、自动签到、手动签到共用。
//
// 为什么单独成一个包而不是各写一份：余额写回要同时更新 credits 与 credits_at，
// 且失败时不能覆盖旧值。三处调用点各写一遍迟早会有一处漏掉某个字段或误把
// 失败当成 0 覆盖掉，导致面板上的积分忽高忽低。
package creditsync

import (
	"context"
	"errors"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// Refresher 是刷新余额所需的最小依赖，避免直接依赖 relay.Refresher 造成循环引用。
type Refresher interface {
	EnsureFresh(ctx context.Context, acc *store.Account) (string, error)
}

type Balancer struct {
	ST     *store.Store
	Client *raccoon.Client
	Auth   Refresher
}

// Refresh 查询并写回余额。返回最新余额。
//
//   - 凭证失效时不写库，保留旧值——把失效当成「余额 0」会让号在面板上凭空消失
//   - 网络抖动同样不写库，下次签到或手动查询自然重试
//   - 成功时同时更新 credits 与 credits_at，保证「这个余额是什么时候的」可追溯
func (b *Balancer) Refresh(ctx context.Context, acc *store.Account) (int64, error) {
	if acc == nil {
		return 0, errors.New("账号为空")
	}
	token, err := b.Auth.EnsureFresh(ctx, acc)
	if err != nil {
		return 0, err
	}
	credits, err := b.Client.Balance(ctx, token, identityOf(acc))
	if err != nil {
		return 0, err
	}
	at := time.Now().UnixMilli()
	if err := b.ST.UpdateAccount(ctx, acc.ID, store.AccountPatch{
		Credits:   &credits,
		CreditsAt: &at,
	}); err != nil {
		return credits, err
	}
	acc.Credits, acc.CreditsAt = credits, at
	return credits, nil
}

func identityOf(acc *store.Account) raccoon.Identity {
	return raccoon.IdentityFor(acc.UID, acc.DeviceID, acc.OSVersion, acc.Platform,
		acc.ClientChannel, acc.OfficeIdentity, acc.Language)
}
