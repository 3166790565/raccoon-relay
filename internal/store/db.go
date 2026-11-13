package store

import (
	"database/sql"
	"fmt"
	"os"
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
	// 开库前先自检：SQLite 的 "unable to open database file (14)" 只表明写不了，
	// 不说明是谁写不了、该怎么修。在容器里这几乎总是 bind mount 属主问题。
	if err := checkWritable(dir); err != nil {
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

// checkWritable 实测数据目录能否创建文件，失败时给出可直接照做的修复命令。
// 只做探测，不留下任何文件。
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".writecheck-*")
	if err == nil {
		name := f.Name()
		f.Close()
		os.Remove(name)
		return nil
	}
	return fmt.Errorf(`数据目录 %s 不可写：%v

容器里出现这个错误，几乎都是 bind mount 的属主问题：
镜像里的 chown 在构建时就完成了，挂载宿主目录会覆盖掉它，
容器内的进程（uid 10001 relay）拿不到写权限。

修复（宿主机上执行）：
  sudo chown -R 10001:10001 <你的数据目录>

或者用最新镜像，入口脚本会在启动时自动修正属主。`, dir, err)
}

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
