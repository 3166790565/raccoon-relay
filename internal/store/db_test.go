package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 目录可写时自检必须放行，且不留下任何探测文件。
func TestCheckWritablePassesOnWritableDir(t *testing.T) {
	dir := t.TempDir()
	if err := checkWritable(dir); err != nil {
		t.Fatalf("可写目录应通过自检: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("自检不应留下文件，实际残留 %d 个", len(entries))
	}
}

// 目录不存在时自检失败，且错误里要给出可直接执行的修复命令。
func TestCheckWritableFailsOnMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nope")
	err := checkWritable(dir)
	if err == nil {
		t.Fatal("不存在的目录应自检失败")
	}
	msg := err.Error()
	if !strings.Contains(msg, "chown -R 10001:10001") {
		t.Fatalf("错误信息应给出修复命令，实际: %s", msg)
	}
	if !strings.Contains(msg, dir) {
		t.Fatalf("错误信息应包含实际路径 %s，实际: %s", dir, msg)
	}
}

// 目录存在但只读时自检失败（Windows 上权限模型不同，跳过）。
func TestCheckWritableFailsOnReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 不受目录权限限制")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip("当前文件系统不支持 chmod")
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	err := checkWritable(dir)
	if err == nil {
		t.Skip("当前平台下只读目录仍可写（Windows/NTFS 常见）")
	}
	if !strings.Contains(err.Error(), "不可写") {
		t.Fatalf("错误信息应说明不可写: %v", err)
	}
}

// Open 在不可写目录上应返回自检错误，而不是 SQLite 的原始错误码。
func TestOpenReportsFriendlyError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing-subdir")
	_, err := Open(dir, "k", false)
	if err == nil {
		t.Fatal("不可写目录上 Open 应失败")
	}
	if strings.Contains(err.Error(), "unable to open database file") {
		t.Fatalf("不应把 SQLite 原始错误直接抛给用户: %v", err)
	}
	if !strings.Contains(err.Error(), "不可写") {
		t.Fatalf("应是可读的权限错误: %v", err)
	}
}

// 老库升级：accounts 表没有 max_concurrent 时，Open 应自动补列且保留原有数据。
func TestOpenAddsMaxConcurrentToOldDB(t *testing.T) {
	dir := t.TempDir()
	// 用原生驱动建一个「旧版」accounts 表（列与 v1.2 一致）。
	raw, err := sql.Open("sqlite", filepath.Join(dir, "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE accounts (
		id TEXT PRIMARY KEY, uid TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
		note TEXT NOT NULL DEFAULT '', token TEXT NOT NULL DEFAULT '',
		refresh_token TEXT NOT NULL DEFAULT '', token_exp INTEGER NOT NULL DEFAULT 0,
		credits INTEGER NOT NULL DEFAULT -1, credits_at INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'online', cool_until INTEGER NOT NULL DEFAULT 0,
		cool_reason TEXT NOT NULL DEFAULT '', expires_at INTEGER NOT NULL DEFAULT 0,
		device_id TEXT NOT NULL DEFAULT '', device_name TEXT NOT NULL DEFAULT '',
		os_version TEXT NOT NULL DEFAULT '', office_identity TEXT NOT NULL DEFAULT '',
		platform TEXT NOT NULL DEFAULT '', client_channel TEXT NOT NULL DEFAULT '',
		language TEXT NOT NULL DEFAULT 'zh', meta TEXT NOT NULL DEFAULT '{}',
		priority INTEGER NOT NULL DEFAULT 0, enabled INTEGER NOT NULL DEFAULT 1,
		last_checkin_day TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL DEFAULT 0, last_used INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO accounts (id, uid, token) VALUES ('a1','u1','enc:plain:tok')`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	s, err := Open(dir, "", true)
	if err != nil {
		t.Fatalf("老库升级失败: %v", err)
	}
	defer s.Close()

	got, err := s.GetAccount(context.Background(), "a1")
	if err != nil || got == nil {
		t.Fatalf("旧数据应保留: %v", err)
	}
	if got.MaxConcurrent != 0 {
		t.Fatalf("新列默认应为 0，实际 %d", got.MaxConcurrent)
	}
	v := int64(4)
	if err := s.UpdateAccount(context.Background(), "a1", AccountPatch{MaxConcurrent: &v}); err != nil {
		t.Fatalf("补列后应可写入: %v", err)
	}
}
