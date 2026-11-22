package store

import (
	"context"
	"testing"
)

// 外部渠道凭据的写入与读取：整组字段加密落库、原样取回。
func TestExternalCredentialRoundTrip(t *testing.T) {
	s, err := Open(t.TempDir(), "k", false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	acc := &Account{
		UID: "u-ext", Name: "ext", Provider: ProviderAutoClaw,
		ExtToken: "acc-1", ExtTokenExp: 111,
		ExtRefreshToken: "ref-1", ExtRefreshExp: 222,
		ExtSite: StationCN, ExtExtra: `{"a":1}`,
		Enabled: true,
	}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExtToken != "acc-1" || got.ExtTokenExp != 111 ||
		got.ExtRefreshToken != "ref-1" || got.ExtRefreshExp != 222 ||
		got.ExtSite != StationCN || got.ExtExtra != `{"a":1}` {
		t.Fatalf("凭据未原样取回: %+v", got)
	}
	if got.ProviderName() != "AutoClaw" || !got.IsExternal() || got.Station() != StationCN {
		t.Fatalf("供应商/站点判断不对: %+v", got)
	}
}

// CAS 写回：版本号变了就拒绝写，避免多副本互相覆盖。
func TestReplaceExtCredentialCAS(t *testing.T) {
	s, err := Open(t.TempDir(), "k", false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	acc := &Account{UID: "u-cas", Provider: ProviderCodeArts, ExtToken: "v0", Enabled: true}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	read, _ := s.GetAccount(ctx, acc.ID)
	if read.ExtRev != 0 {
		t.Fatalf("新号 rev 应为 0，实际 %d", read.ExtRev)
	}

	ok, err := s.ReplaceExtCredential(ctx, acc.ID, 0, ExtCredential{
		Token: "v1", TokenExp: 1, RefreshToken: "r1", Site: StationGlobal, Extra: "x",
	})
	if err != nil || !ok {
		t.Fatalf("首次 CAS 应成功: ok=%v err=%v", ok, err)
	}
	// 用同一个（已过期的）版本号再写一次，必须失败。
	ok, err = s.ReplaceExtCredential(ctx, acc.ID, 0, ExtCredential{Token: "v2"})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("旧版本号不应写入成功")
	}
	after, _ := s.GetAccount(ctx, acc.ID)
	if after.ExtToken != "v1" || after.ExtRev != 1 {
		t.Fatalf("CAS 失败后凭据不该被覆盖: token=%q rev=%d", after.ExtToken, after.ExtRev)
	}
}
