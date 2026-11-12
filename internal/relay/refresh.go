package relay

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// Refresher 负责按账号单飞刷新 token。access 约 3h，默认只在临到 5 分钟内才刷，
// 贴官方惰性语义：抢在客户端之前刷会把同一个 refresh_token 轮换掉，反而两边都掉登录。
type Refresher struct {
	ST     *store.Store
	Client *raccoon.Client
	Window time.Duration

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func NewRefresher(st *store.Store, c *raccoon.Client, window time.Duration) *Refresher {
	if window <= 0 {
		window = 5 * time.Minute
	}
	return &Refresher{ST: st, Client: c, Window: window, locks: map[string]*sync.Mutex{}}
}

func (r *Refresher) lockFor(id string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.locks[id]
	if !ok {
		l = &sync.Mutex{}
		r.locks[id] = l
	}
	return l
}

// EnsureFresh 在 access 临期时刷新，返回可用的 token。刷新被拒则返回错误。
func (r *Refresher) EnsureFresh(ctx context.Context, acc *store.Account) (string, error) {
	if !needsRefresh(acc, r.Window) {
		return acc.Token, nil
	}
	l := r.lockFor(acc.ID)
	l.Lock()
	defer l.Unlock()

	// 双检：等锁期间可能已被别的请求刷过
	fresh, err := r.ST.GetAccount(ctx, acc.ID)
	if err == nil && fresh != nil && !needsRefresh(fresh, r.Window) {
		acc.Token, acc.RefreshToken, acc.TokenExp = fresh.Token, fresh.RefreshToken, fresh.TokenExp
		return fresh.Token, nil
	}
	return r.RefreshNow(ctx, acc)
}

// RefreshNow 无视过期时间强制刷新。
func (r *Refresher) RefreshNow(ctx context.Context, acc *store.Account) (string, error) {
	if strings.TrimSpace(acc.RefreshToken) == "" {
		return "", &raccoon.UpstreamError{Status: 401, Auth: true, Message: "无 refreshToken，请重新导入凭据"}
	}
	idn := identityOf(acc)
	res, err := r.Client.Refresh(ctx, idn, acc.RefreshToken)
	if err != nil {
		return "", err
	}
	exp := raccoon.TokenExpiryMillis(res.Token)
	if err := r.ST.UpdateAccount(ctx, acc.ID, store.AccountPatch{
		Token:        &res.Token,
		RefreshToken: &res.RefreshToken,
		TokenExp:     &exp,
	}); err != nil {
		return "", err
	}
	acc.Token, acc.RefreshToken, acc.TokenExp = res.Token, res.RefreshToken, exp
	return res.Token, nil
}

func needsRefresh(acc *store.Account, window time.Duration) bool {
	if acc.TokenExp == 0 {
		return false // 解不出 exp 就当作可用，让上游 401 来定
	}
	return time.Now().Add(window).UnixMilli() >= acc.TokenExp
}

// identityOf 由账号行组装指纹。
func identityOf(acc *store.Account) raccoon.Identity {
	return raccoon.IdentityFor(
		acc.UID,
		acc.DeviceID,
		acc.OSVersion,
		acc.Platform,
		acc.ClientChannel,
		acc.OfficeIdentity,
		acc.Language,
	)
}
