package logmgr

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/3166790565/raccoon-relay/internal/store"
)

// newTest 构造一个底层输出被丢弃的 Manager：测试不往终端刷日志。
func newTest(cfg Config, st *store.Store) *Manager {
	m := New(cfg, st)
	m.base = io.Discard
	return m
}

// write 直接调用 Writer，绕过全局 logger，避免并行测试互相干扰。
func write(t *testing.T, m *Manager, s string) {
	t.Helper()
	if _, err := m.Write([]byte(s)); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(raw)
}

// 配置归一化：越界值必须收敛到安全默认，否则轮转逻辑会除以 0 或永不清理。
func TestConfigNormalize(t *testing.T) {
	c := Config{}.Normalize()
	if c.MaxSizeMB <= 0 || c.MaxFiles <= 0 || c.CleanInterval <= 0 {
		t.Fatalf("零值应归一化为正数: %+v", c)
	}
	if c.FileRetainDays < 0 || c.DBRetainDays < 0 {
		t.Fatalf("负数保留天数应归零: %+v", c)
	}
}

// 超过大小上限时轮转：旧内容进归档文件，当前文件重新从空开始。
func TestRotateOnSize(t *testing.T) {
	dir := t.TempDir()
	m := newTest(Config{Enable: true, Dir: dir, MaxSizeMB: 1, MaxFiles: 5}, nil)
	defer m.Close()

	// 单文件上限 1MB；写 1.2MB 必然触发轮转（按字节数判断，不看行边界）
	chunk := strings.Repeat("x", 64*1024)
	for i := 0; i < 20; i++ {
		write(t, m, chunk)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var archived int
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "relay-") {
			archived++
		}
	}
	if archived == 0 {
		t.Fatalf("超过大小上限后应产生归档文件，实际目录: %v", names(entries))
	}

	current := filepath.Join(dir, currentName)
	if fi, err := os.Stat(current); err != nil {
		t.Fatalf("当前文件应存在: %v", err)
	} else if fi.Size() > 1<<20 {
		t.Fatalf("当前文件不应超过上限，实际 %d 字节", fi.Size())
	}
}

// 归档数量超过上限时，从最旧的开始删除。
func TestPruneByMaxFiles(t *testing.T) {
	dir := t.TempDir()
	m := newTest(Config{Enable: true, Dir: dir, MaxSizeMB: 1, MaxFiles: 2}, nil)
	defer m.Close()

	// 直接构造 5 个归档文件，mod 时间依次递增
	for i := 0; i < 5; i++ {
		p := filepath.Join(dir, "relay-2026010"+string(rune('1'+i))+"-000000.log")
		if err := os.WriteFile(p, []byte("old"), 0o640); err != nil {
			t.Fatal(err)
		}
		ts := time.Now().Add(time.Duration(i) * time.Hour)
		_ = os.Chtimes(p, ts, ts)
	}

	m.mu.Lock()
	removed, _ := m.pruneLocked(time.Now())
	m.mu.Unlock()

	if removed != 3 {
		t.Fatalf("应删除 3 个最旧归档，实际 %d", removed)
	}
	entries, _ := os.ReadDir(dir)
	var left []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "relay-") {
			left = append(left, e.Name())
		}
	}
	if len(left) != 2 {
		t.Fatalf("应只保留 2 个归档，实际 %v", left)
	}
	for _, n := range left {
		// 保留的必须是较新的那两个（04 和 05），最旧的 01/02/03 应被删
		if strings.Contains(n, "-01-") || strings.Contains(n, "-02-") || strings.Contains(n, "-03-") {
			t.Fatalf("最旧的归档应被删除，实际留下 %s", n)
		}
	}
}

// 按保留天数删除：早于 cutoff 的归档被删，当前文件绝不能被误删。
func TestPruneByRetainDays(t *testing.T) {
	dir := t.TempDir()
	m := newTest(Config{Enable: true, Dir: dir, MaxFiles: 100, FileRetainDays: 7}, nil)
	defer m.Close()

	old := filepath.Join(dir, "relay-20200101-000000.log")
	fresh := filepath.Join(dir, "relay-20260101-000000.log")
	cur := filepath.Join(dir, currentName)
	for _, p := range []string{old, fresh, cur} {
		if err := os.WriteFile(p, []byte("data"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	longAgo := time.Now().AddDate(0, 0, -30)
	_ = os.Chtimes(old, longAgo, longAgo)

	m.mu.Lock()
	removed, _ := m.pruneLocked(time.Now())
	m.mu.Unlock()

	if removed != 1 {
		t.Fatalf("应只删 1 个过期归档，实际 %d", removed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("过期归档应被删除")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("未过期归档不应被删除")
	}
	if _, err := os.Stat(cur); err != nil {
		t.Fatal("当前文件绝不能删除")
	}
}

// 跨天时旧当前文件应被归档，而不是让新日志接在旧文件后面。
// 清理要把运行事件一起按流水保留天数删掉，否则排障数据会比流水先滚掉。
func TestCleanPurgesRuntimeEvents(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(dir, "", true)
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer st.Close()

	old := time.Now().AddDate(0, 0, -10).UnixMilli()
	if err := st.AddEvent(ctx, store.RuntimeEvent{TS: old, Message: "旧事件"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddEvent(ctx, store.RuntimeEvent{Message: "新事件"}); err != nil {
		t.Fatal(err)
	}

	m := newTest(Config{Enable: false, Dir: dir, DBRetainDays: 7, MaxSizeMB: 1, MaxFiles: 5}, st)
	defer m.Close()
	res := m.CleanNow(ctx)
	if res.RemovedEvents != 1 {
		t.Fatalf("应删除 1 条过期事件，实际 %d", res.RemovedEvents)
	}
	if count, _, _ := st.EventStats(ctx); count != 1 {
		t.Fatalf("应剩 1 条事件，实际 %d", count)
	}
}

func TestArchiveStaleCurrentFile(t *testing.T) {
	dir := t.TempDir()
	cur := filepath.Join(dir, currentName)
	if err := os.WriteFile(cur, []byte("yesterday"), 0o640); err != nil {
		t.Fatal(err)
	}
	old := time.Now().AddDate(0, 0, -1)
	_ = os.Chtimes(cur, old, old)

	m := newTest(Config{Enable: true, Dir: dir, MaxFiles: 5}, nil)
	defer m.Close()
	write(t, m, "today")

	entries, _ := os.ReadDir(dir)
	var archived int
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "relay-") {
			archived++
		}
	}
	if archived != 1 {
		t.Fatalf("昨天的当前文件应被归档，实际归档数 %d", archived)
	}
	if got := readFile(t, cur); got != "today" {
		t.Fatalf("当前文件应只包含今天的日志，实际 %q", got)
	}
}

// 关闭文件日志时不创建目录、不写文件，但仍然透传给底层 writer。
func TestDisabledWritesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "should-not-exist")
	m := newTest(Config{Enable: false, Dir: dir}, nil)
	defer m.Close()
	write(t, m, "hello")

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("关闭时不应创建日志目录: %v", err)
	}
}

// 文件名必须在目录内，路径穿越要被拒绝。
func TestReadFileRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	m := newTest(Config{Enable: true, Dir: dir}, nil)
	defer m.Close()
	if _, err := m.ReadFile("../relay.db"); err == nil {
		t.Fatal("应拒绝路径穿越")
	}
}

func names(entries []os.DirEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
