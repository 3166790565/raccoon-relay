package codearts

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/3166790565/raccoon-relay/internal/raccoon"
	"github.com/3166790565/raccoon-relay/internal/relay"
	"github.com/3166790565/raccoon-relay/internal/store"
)

// Upstream 把 CodeArts 能力包成 relay.ExtUpstream，并额外提供额度与签到。
type Upstream struct {
	C  *Client
	ST *store.Store
}

func NewUpstream(c *Client, st *store.Store) *Upstream { return &Upstream{C: c, ST: st} }

var _ relay.ExtUpstream = (*Upstream)(nil)

// EnsureCredential 在临时凭据临期时续期，并用 ext_rev 做 CAS 写回。
//
// 与 AutoClaw 同一套多副本策略：进程内单飞 + 库里版本号。STS 的 refresh_token
// 同样一次性，并发续期会让后写者覆盖掉有效的新凭据。
func (u *Upstream) EnsureCredential(ctx context.Context, acc *store.Account) error {
	cred, err := DecodeCred(acc)
	if err != nil {
		return &raccoon.UpstreamError{Status: 401, Auth: true, Message: err.Error()}
	}
	if !cred.NeedsRefresh(time.Now()) {
		return nil
	}
	if cred.RefreshToken == "" {
		return nil // 无续期材料（例如 IDE 导入的号），到期后由转发层按 401 处理
	}

	mu := codeartsRefreshLock(acc.ID)
	mu.Lock()
	defer mu.Unlock()

	// 双检：只有「已知到期且未临期」才算别人已经续好。ExpiryMillis()==0 表示
	// 到期未知，不能被当成「永不过期」——那会让一次续期把自己判成成功。
	if fresh, gerr := u.ST.GetAccount(ctx, acc.ID); gerr == nil && fresh != nil {
		if fc, derr := DecodeCred(fresh); derr == nil &&
			fc.ExpiryMillis() > time.Now().Add(2*time.Minute).UnixMilli() {
			acc.ExtExtra, acc.ExtRev = fresh.ExtExtra, fresh.ExtRev
			return nil
		}
	}

	next, err := u.C.RefreshCredential(ctx, cred)
	if err != nil {
		return err
	}
	ok, werr := u.ST.ReplaceExtCredential(ctx, acc.ID, acc.ExtRev, store.ExtCredential{
		Token:        next.AccessKeyID + ":" + next.ExpiresAt,
		TokenExp:     next.ExpiryMillis(),
		RefreshToken: next.RefreshToken,
		Site:         acc.Station(),
		Extra:        mustJSON(next),
	})
	if werr != nil {
		return werr
	}
	if !ok {
		if fresh, gerr := u.ST.GetAccount(ctx, acc.ID); gerr == nil && fresh != nil {
			acc.ExtExtra, acc.ExtRev = fresh.ExtExtra, fresh.ExtRev
		}
		return nil
	}
	acc.ExtExtra = mustJSON(next)
	acc.ExtRev++
	return nil
}

func mustJSON(c Credential) string {
	raw, _ := json.Marshal(c)
	return string(raw)
}

// OpenChat 实现 relay.ExtUpstream。
func (u *Upstream) OpenChat(ctx context.Context, acc *store.Account, body map[string]any) (*raccoon.ChatStream, error) {
	return u.C.OpenChat(ctx, acc, body)
}

// Models 实现 relay.ExtUpstream。
func (u *Upstream) Models(ctx context.Context, acc *store.Account) []raccoon.Model {
	if live := u.C.FetchModels(ctx, acc); len(live) > 0 {
		return ToRaccoon(live)
	}
	return ToRaccoon(FallbackModels())
}

// RefreshQuota 拉额度并写回账号。
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
	credits := int64(q.Remain)
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

// StoredQuota 返回上次落库的额度快照。
func StoredQuota(acc *store.Account) *Quota { return UnmarshalQuota(acc.SNPool) }

// Checkin 实现调度器的签到需求。
func (u *Upstream) Checkin(ctx context.Context, acc *store.Account) (bool, bool, int64, string) {
	if err := u.EnsureCredential(ctx, acc); err != nil {
		return false, false, 0, err.Error()
	}
	res := u.C.Checkin(ctx, acc)
	return res.OK, res.Already, res.Reward, res.Message
}

// codeartsRefreshLock 返回该账号的进程内续期锁。
func codeartsRefreshLock(id string) *sync.Mutex {
	return lockForAccount(id)
}
