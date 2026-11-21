package store

import (
	"context"
	"testing"
	"time"
)

func TestVerifyKeyCachesHitAndInvalidatesOnDelete(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	k, secret, err := s.CreateKey(ctx, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if !s.VerifyKey(ctx, secret) {
		t.Fatal("新 Key 应校验通过")
	}
	// 命中缓存：直接删掉库里的行，仍然应当通过（证明没有再查库）。
	if _, err := s.exec(ctx, `DELETE FROM api_keys WHERE id = ?`, k.ID); err != nil {
		t.Fatal(err)
	}
	if !s.VerifyKey(ctx, secret) {
		t.Fatal("缓存期内应直接命中，不应重新查库")
	}
	// DeleteKey 路径会主动失效；这里手工模拟同样的失效后应立刻失败。
	s.invalidateKey(KeyHash(secret))
	if s.VerifyKey(ctx, secret) {
		t.Fatal("缓存失效后应校验失败")
	}
}

func TestVerifyKeyDoesNotCacheMiss(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	_, secret, err := s.CreateKey(ctx, "k1")
	if err != nil {
		t.Fatal(err)
	}
	wrong := secret + "x"
	if s.VerifyKey(ctx, wrong) {
		t.Fatal("错误 Key 不应通过")
	}
	// 新增的 Key 不需要等 TTL：miss 不缓存。
	_, secret2, err := s.CreateKey(ctx, "k2")
	if err != nil {
		t.Fatal(err)
	}
	if !s.VerifyKey(ctx, secret2) {
		t.Fatal("新创建的 Key 应立即可用")
	}
}

func TestLastUsedWriteIsThrottled(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	_, secret, err := s.CreateKey(ctx, "k1")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.VerifyKey(ctx, secret)
	var first int64
	if err := s.queryRow(ctx, `SELECT last_used FROM api_keys`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if first == 0 {
		t.Fatal("首次校验应写入 last_used")
	}
	// 立即再次校验：节流窗口内不应再写。
	time.Sleep(5 * time.Millisecond)
	_ = s.VerifyKey(ctx, secret)
	var second int64
	_ = s.queryRow(ctx, `SELECT last_used FROM api_keys`).Scan(&second)
	if second != first {
		t.Fatalf("60s 内不应重复写 last_used（%d → %d）", first, second)
	}
}

func TestListAccountsSnapshotIsIsolated(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if err := s.CreateAccount(ctx, &Account{UID: "u1", Name: "甲", Token: "tok", Enabled: true,
		Meta: map[string]any{"a": 1}}); err != nil {
		t.Fatal(err)
	}

	first, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 修改调用方拿到的那份，不应污染缓存。
	first[0].Name = "被改了"
	first[0].Meta["a"] = 999

	second, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// JSON 反序列化后数字是 float64，比较时按数值比。
	if second[0].Name != "甲" || second[0].Meta["a"] != float64(1) {
		t.Fatalf("快照必须是深拷贝，实际: name=%q meta=%v", second[0].Name, second[0].Meta)
	}
}

func TestAccountWriteInvalidatesSnapshot(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	a := &Account{UID: "u1", Token: "tok", Enabled: true}
	if err := s.CreateAccount(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListAccounts(ctx); err != nil { // 填充快照
		t.Fatal(err)
	}
	name := "新名字"
	if err := s.UpdateAccount(ctx, a.ID, AccountPatch{Name: &name}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Name != "新名字" {
		t.Fatalf("写后快照应失效，实际还是 %q", got[0].Name)
	}
}

func TestAddUsageAsyncFlushes(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	for i := 0; i < 5; i++ {
		s.AddUsageAsync(UsageLog{Model: "m", Status: 200})
	}
	s.FlushUsage()
	var n int64
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM usage_logs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("异步写入后应有 5 条，实际 %d", n)
	}
}