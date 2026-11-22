package autoclaw

import (
	"context"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// 回归：到期未知（Expires == 0）的凭据不能被双检当成「已经续好」。
//
// 之前的写法是 `Expires == 0 || Expires > now`，结果一次续期请求返回成功、
// 但上游实际拒绝时，第二次进来会把同一份旧凭据判成有效，签到/刷新静默成功。
func TestEnsureCredentialDoesNotTreatUnknownExpiryAsFresh(t *testing.T) {
	st, err := store.Open(t.TempDir(), "k", false)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	acc := &store.Account{
		UID: "u1", Provider: store.ProviderAutoClaw,
		ExtToken: "access-only", ExtRefreshToken: "refresh-only",
		// 故意不给到期时刻。
		Enabled: true,
	}
	if err := st.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}

	// 用一个必然连不上的地址，续期会失败；如果双检把未知到期当成有效，
	// 这里就会返回 nil（假的成功）。
	c := NewClient(time.Second)
	c.GlobalHost = "http://127.0.0.1:1"
	c.CNHost = "http://127.0.0.1:1"
	u := NewUpstream(c, st)

	read, _ := st.GetAccount(ctx, acc.ID)
	if err := u.EnsureCredential(ctx, read); err == nil {
		t.Fatal("上游不可达时应报错，而不是把未知到期当作有效")
	}
}
