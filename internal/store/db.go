package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

type Store struct {
	db     *sql.DB
	cipher *Cipher
	mu     sync.Mutex
}

func Open(dir, secret string, allowPlaintext bool) (*Store, error) {
	c, err := NewCipher(secret, allowPlaintext)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "relay.db")
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// 纯 Go 驱动并发写会锁竞争，串行化会话；读多写少的场景够用。
	db.SetMaxOpenConns(1)
	s := &Store{db: db, cipher: c}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	for _, stmt := range schema {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("建表失败: %w", err)
		}
	}
	var ver int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		return err
	}
	if ver < schemaVersion {
		if _, err := s.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) DB() *sql.DB { return s.db }
