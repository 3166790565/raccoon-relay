// Package creditsync 提供「刷新账号余额」这一件事，供添加账号、自动签到、手动签到共用。
//
// 为什么单独成一个包而不是各写一份：余额写回要同时更新 credits 与 credits_at，
// 且失败时不能覆盖旧值。三处调用点各写一遍迟早会有一处漏掉某个字段或误把
// 失败当成 0 覆盖掉，导致面板上的积分忽高忽低。
package creditsync

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// Refresher 是刷新余额所需的最小依赖，避免直接依赖 relay.Refresher 造成循环引用。
type Refresher interface {
	EnsureFresh(ctx context.Context, acc *store.Account) (string, error)
}

// SNBalance 是日日新余额查询所需的最小能力，由 sensenova.Upstream 满足。
type SNBalance interface {
	// FetchUsageFor 确保登录票可用后读积分池，返回主池剩余量与完整池的
	// JSON（供面板展示），并已把刷新后的登录票写回库。
	FetchUsageFor(ctx context.Context, acc *store.Account) (remaining float64, stored string, err error)
}

type Balancer struct {
	ST     *store.Store
	Client *raccoon.Client
	Auth   Refresher
	// SN 是日日新余额来源，nil 表示不支持日日新。
	SN SNBalance
	// OnUpdated 在余额写库成功后调用，用来立刻按积分阈值重算账号状态。
	// 可选：为空时只写余额。
	OnUpdated func(ctx context.Context)
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
	if acc.IsSenseNova() {
		return b.refreshSenseNova(ctx, acc)
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
	if b.OnUpdated != nil {
		b.OnUpdated(ctx)
	}
	return credits, nil
}

// refreshSenseNova 用控制台登录票读日日新积分池，写回 credits。
//
// 与小浣熊不同的是，日日新的 token-plan 端点没有可查询的单一「余额」：
// 额度是按 5 小时 / 本周窗口滚动的池子。这里取主池 5 小时窗口的剩余量
// 作为 credits（与面板其它号同口径），完整池数据另存到 sn_pool 供面板展示。
func (b *Balancer) refreshSenseNova(ctx context.Context, acc *store.Account) (int64, error) {
	if b.SN == nil {
		return 0, errors.New("日日新余额查询未配置")
	}
	remaining, stored, err := b.SN.FetchUsageFor(ctx, acc)
	if err != nil {
		return 0, err
	}
	credits := int64(math.Round(remaining))
	at := time.Now().UnixMilli()
	patch := store.AccountPatch{Credits: &credits, CreditsAt: &at, SNPool: &stored, SNPoolAt: &at}
	if err := b.ST.UpdateAccount(ctx, acc.ID, patch); err != nil {
		return credits, err
	}
	acc.Credits, acc.CreditsAt, acc.SNPool, acc.SNPoolAt = credits, at, stored, at
	if b.OnUpdated != nil {
		b.OnUpdated(ctx)
	}
	return credits, nil
}

func identityOf(acc *store.Account) raccoon.Identity {
	return raccoon.IdentityFor(acc.UID, acc.DeviceID, acc.OSVersion, acc.Platform,
		acc.ClientChannel, acc.OfficeIdentity, acc.Language)
}

// RefreshStale 刷新「余额过旧」的账号，返回刷新成功条数。
//
// 为什么需要它：余额只在三个时刻更新——签到、新号导入、手动点「查余额」。
// 签到每天才一次、而且失败的号不会刷；日日新干脆没有签到。于是面板上的余额
// 会长期停在旧值，额度耗尽后冷却到期也无法确认是否真的恢复。这条巡检把
// 所有启用中、且读数超过 maxAge 没更新的号挨个查一遍。
//
//   - 按账号各自的余额时间戳判断新旧：小浣熊/外部渠道看 CreditsAt，
//     日日新看 SNPoolAt（它才是池读数的来源）；
//   - 停用号与 relogin 号跳过：前者不该产生上游请求，后者的凭据已经不能用；
//   - 单个账号失败静默跳过：余额是辅助信息，一次上游抖动不该中断整轮巡检。
//
// 调用方负责用分布式锁保证多副本下只有一份在跑（见 cmd/relay 的 pool-tick）。
func (b *Balancer) RefreshStale(ctx context.Context, maxAge time.Duration) int {
	if b == nil || b.ST == nil {
		return 0
	}
	accounts, err := b.ST.ListAccounts(ctx)
	if err != nil {
		return 0
	}
	now := time.Now().UnixMilli()
	minGap := maxAge.Milliseconds()
	n := 0
	for _, a := range accounts {
		if !a.Enabled || a.Status == store.StatusRelogin {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		at := a.CreditsAt
		if a.IsSenseNova() {
			at = a.SNPoolAt
		}
		if at > 0 && now-at < minGap {
			continue
		}
		// 没有对应渠道能力时 Refresh 会直接报错，跳过即可。
		if _, err := b.Refresh(ctx, a); err == nil {
			n++
		}
	}
	return n
}
