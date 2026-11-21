package cache

import (
	"context"
	"testing"
	"time"
)

// 未配置 Redis 时：锁互斥生效、释放后可重取；KV 有过期。
func TestLocalLockAndKV(t *testing.T) {
	ctx := context.Background()
	c := New("", "test:")
	if c.Enabled() {
		t.Fatal("未配置 URL 时不应启用 Redis")
	}

	release, ok := c.Lock(ctx, "job", time.Minute)
	if !ok {
		t.Fatal("本地模式首次加锁应成功")
	}
	if _, ok := c.Lock(ctx, "job", time.Minute); ok {
		t.Fatal("同名锁未释放时不应再次成功")
	}
	release()
	if _, ok := c.Lock(ctx, "job", time.Minute); !ok {
		t.Fatal("释放后应能重新加锁")
	}

	c.Set(ctx, "k", "v", 50*time.Millisecond)
	if v, ok := c.Get(ctx, "k"); !ok || v != "v" {
		t.Fatalf("缓存读取失败: %q %v", v, ok)
	}
	time.Sleep(60 * time.Millisecond)
	if _, ok := c.Get(ctx, "k"); ok {
		t.Fatal("过期后不应命中")
	}

	c.Set(ctx, "k2", "v2", time.Minute)
	c.Del(ctx, "k2")
	if _, ok := c.Get(ctx, "k2"); ok {
		t.Fatal("删除后不应命中")
	}
}

// 锁过期后应可被重新获取（模拟进程崩溃未释放）。
func TestLocalLockExpires(t *testing.T) {
	c := New("", "")
	ctx := context.Background()
	if _, ok := c.Lock(ctx, "job", 30*time.Millisecond); !ok {
		t.Fatal("首次应成功")
	}
	time.Sleep(40 * time.Millisecond)
	if _, ok := c.Lock(ctx, "job", time.Minute); !ok {
		t.Fatal("TTL 到期后应能重新获取")
	}
}

// 无法解析的 URL 不应 panic，降级为本地实现。
func TestBadURLFallsBack(t *testing.T) {
	c := New("not-a-url", "")
	if c.Enabled() {
		t.Fatal("非法 URL 不应启用 Redis")
	}
	if _, ok := c.Lock(context.Background(), "x", time.Minute); !ok {
		t.Fatal("降级后本地锁应可用")
	}
}
