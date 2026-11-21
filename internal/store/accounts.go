package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// 账号状态：与桌面号池同口径，选号与冷却都按这四个值判断。
const (
	StatusOnline    = "online"
	StatusCooling   = "cooling"
	StatusExhausted = "exhausted"
	StatusRelogin   = "relogin"
)

// Account 是对外的完整账号视图。Token 字段在 List/Get 时会解密好返回。
type Account struct {
	ID             string
	UID            string
	Name           string
	Note           string
	Token          string
	RefreshToken   string
	TokenExp       int64
	Credits        int64
	CreditsAt      int64
	Status         string
	CoolUntil      int64
	CoolReason     string
	ExpiresAt      int64
	DeviceID       string
	DeviceName     string
	OSVersion      string
	OfficeIdentity string
	Platform       string
	ClientChannel  string
	Language       string
	Meta           map[string]any
	Priority       int64
	Enabled        bool
	LastCheckinDay string
	CreatedAt      int64
	UpdatedAt      int64
	LastUsed       int64
}

const accountCols = `id, uid, name, note, token, refresh_token, token_exp, credits, credits_at,
	status, cool_until, cool_reason, expires_at, device_id, device_name, os_version,
	office_identity, platform, client_channel, language, meta, priority, enabled,
	last_checkin_day, created_at, updated_at, last_used`

// 写入用的字段补丁：nil 表示不改。
type AccountPatch struct {
	Name           *string
	Note           *string
	Token          *string
	RefreshToken   *string
	TokenExp       *int64
	Credits        *int64
	CreditsAt      *int64
	Status         *string
	CoolUntil      *int64
	CoolReason     *string
	ExpiresAt      *int64
	DeviceID       *string
	DeviceName     *string
	OSVersion      *string
	OfficeIdentity *string
	Platform       *string
	ClientChannel  *string
	Language       *string
	Meta           map[string]any
	Priority       *int64
	Enabled        *bool
	LastCheckinDay *string
	LastUsed       *int64
}

func (s *Store) scanAccount(sc interface{ Scan(...any) error }) (*Account, error) {
	var a Account
	var tok, ref, meta string
	var enabled int64
	err := sc.Scan(&a.ID, &a.UID, &a.Name, &a.Note, &tok, &ref, &a.TokenExp,
		&a.Credits, &a.CreditsAt, &a.Status, &a.CoolUntil, &a.CoolReason, &a.ExpiresAt,
		&a.DeviceID, &a.DeviceName, &a.OSVersion, &a.OfficeIdentity, &a.Platform,
		&a.ClientChannel, &a.Language, &meta, &a.Priority, &enabled,
		&a.LastCheckinDay, &a.CreatedAt, &a.UpdatedAt, &a.LastUsed)
	if err != nil {
		return nil, err
	}
	a.Enabled = enabled != 0
	a.Token, err = s.cipher.Decrypt(tok)
	if err != nil {
		return nil, err
	}
	a.RefreshToken, err = s.cipher.Decrypt(ref)
	if err != nil {
		return nil, err
	}
	if meta != "" {
		_ = json.Unmarshal([]byte(meta), &a.Meta)
	}
	if a.Meta == nil {
		a.Meta = map[string]any{}
	}
	return &a, nil
}

// ListAccounts 返回全部账号（已解密）。
//
// 走 1 秒进程内快照：选号在每个请求（以及每次换号重试）时都会调它，
// 而全表读取 + 逐行 AES 解密在热路径上代价很高。
// 所有账号写操作都会失效快照，多副本之间最多滞后 1 秒。
func (s *Store) ListAccounts(ctx context.Context) ([]*Account, error) {
	return s.listAccountsCached(ctx)
}

func (s *Store) listAccountsDB(ctx context.Context) ([]*Account, error) {
	// 并列时按 id 排序：两种方言的并列顺序都不确定，多实例批量 UPDATE
	// 若顺序不一致会放大死锁概率。
	rows, err := s.query(ctx, `SELECT `+accountCols+` FROM accounts ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Account
	for rows.Next() {
		a, err := s.scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetAccount(ctx context.Context, id string) (*Account, error) {
	row := s.queryRow(ctx, `SELECT `+accountCols+` FROM accounts WHERE id = ?`, id)
	a, err := s.scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

func (s *Store) GetByUID(ctx context.Context, uid string) (*Account, error) {
	row := s.queryRow(ctx, `SELECT `+accountCols+` FROM accounts WHERE uid = ?`, uid)
	a, err := s.scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// CreateAccount 插入新号；uid 唯一冲突由调用方先去重。
func (s *Store) CreateAccount(ctx context.Context, a *Account) error {
	now := time.Now().UnixMilli()
	if a.ID == "" {
		a.ID = "acc-" + RandomHex(8)
	}
	if a.Status == "" {
		a.Status = StatusOnline
	}
	if a.Language == "" {
		a.Language = "zh"
	}
	if a.Meta == nil {
		a.Meta = map[string]any{}
	}
	meta, _ := json.Marshal(a.Meta)
	a.CreatedAt, a.UpdatedAt = now, now
	enabled := 0
	if a.Enabled {
		enabled = 1
	}
	_, err := s.exec(ctx, `INSERT INTO accounts (`+accountCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.UID, a.Name, a.Note, s.cipher.Encrypt(a.Token), s.cipher.Encrypt(a.RefreshToken),
		a.TokenExp, a.Credits, a.CreditsAt, a.Status, a.CoolUntil, a.CoolReason, a.ExpiresAt,
		a.DeviceID, a.DeviceName, a.OSVersion, a.OfficeIdentity, a.Platform, a.ClientChannel,
		a.Language, string(meta), a.Priority, enabled, a.LastCheckinDay,
		a.CreatedAt, a.UpdatedAt, a.LastUsed)
	if err == nil {
		s.invalidateAccounts()
	}
	return err
}

func (s *Store) UpdateAccount(ctx context.Context, id string, p AccountPatch) error {
	sets := []string{"updated_at = ?"}
	args := []any{time.Now().UnixMilli()}
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if p.Name != nil {
		add("name", *p.Name)
	}
	if p.Note != nil {
		add("note", *p.Note)
	}
	if p.Token != nil {
		add("token", s.cipher.Encrypt(*p.Token))
	}
	if p.RefreshToken != nil {
		add("refresh_token", s.cipher.Encrypt(*p.RefreshToken))
	}
	if p.TokenExp != nil {
		add("token_exp", *p.TokenExp)
	}
	if p.Credits != nil {
		add("credits", *p.Credits)
	}
	if p.CreditsAt != nil {
		add("credits_at", *p.CreditsAt)
	}
	if p.Status != nil {
		add("status", *p.Status)
	}
	if p.CoolUntil != nil {
		add("cool_until", *p.CoolUntil)
	}
	if p.CoolReason != nil {
		add("cool_reason", *p.CoolReason)
	}
	if p.ExpiresAt != nil {
		add("expires_at", *p.ExpiresAt)
	}
	if p.DeviceID != nil {
		add("device_id", *p.DeviceID)
	}
	if p.DeviceName != nil {
		add("device_name", *p.DeviceName)
	}
	if p.OSVersion != nil {
		add("os_version", *p.OSVersion)
	}
	if p.OfficeIdentity != nil {
		add("office_identity", *p.OfficeIdentity)
	}
	if p.Platform != nil {
		add("platform", *p.Platform)
	}
	if p.ClientChannel != nil {
		add("client_channel", *p.ClientChannel)
	}
	if p.Language != nil {
		add("language", *p.Language)
	}
	if p.Meta != nil {
		meta, _ := json.Marshal(p.Meta)
		add("meta", string(meta))
	}
	if p.Priority != nil {
		add("priority", *p.Priority)
	}
	if p.Enabled != nil {
		b := 0
		if *p.Enabled {
			b = 1
		}
		add("enabled", b)
	}
	if p.LastCheckinDay != nil {
		add("last_checkin_day", *p.LastCheckinDay)
	}
	if p.LastUsed != nil {
		add("last_used", *p.LastUsed)
	}
	args = append(args, id)
	_, err := s.exec(ctx, `UPDATE accounts SET `+joinComma(sets)+` WHERE id = ?`, args...)
	if err == nil {
		s.invalidateAccounts()
	}
	return err
}

func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.deleteAccountRows(ctx, tx, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.invalidateAccounts()
	return nil
}

// deleteAccountRows 删掉账号及挂在它下面的冷却与签到记录。
// 三张表必须一起删：只删 accounts 会留下孤儿行，之后同 uid 重新导入时
// 旧冷却会把新号立刻压回冷却状态。
//
// 走 execTx 而不是裸 tx.ExecContext：Postgres 下占位符需要 rebind。
func (s *Store) deleteAccountRows(ctx context.Context, tx *sql.Tx, id string) error {
	for _, q := range []string{
		`DELETE FROM model_cooldowns WHERE account_id = ?`,
		`DELETE FROM account_cooldowns WHERE account_id = ?`,
		`DELETE FROM checkin_history WHERE account_id = ?`,
		`DELETE FROM accounts WHERE id = ?`,
	} {
		if _, err := s.execTx(ctx, tx, q, id); err != nil {
			return err
		}
	}
	return nil
}

// DeadReason 判断账号是否已经救不回来，返回命中原因（空串表示仍然可用）。
//
// 两种口径：
//   - 凭证失效：状态为 relogin。refresh_token 也刷不动，只能靠重新登录拿新凭据；
//   - 订阅过期：expires_at 已过。凭据也许还能用，但这个号不会再产出可用的额度。
//
// 积分耗尽（exhausted）不算：它会在次日 04:00 自动复活，删了就再也回不来。
// 冷却（cooling）更不算，那只是临时限流。
func (a *Account) DeadReason(nowMS int64) string {
	if a.Status == StatusRelogin {
		return "凭证失效"
	}
	if a.ExpiresAt > 0 && a.ExpiresAt <= nowMS {
		return "订阅已过期"
	}
	return ""
}

// DeadAccount 是待清理账号的最小视图，供面板预览与确认。
type DeadAccount struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Reason     string `json:"reason"`
	ExpiresAt  int64  `json:"expiresAt"`
	CoolReason string `json:"coolReason"`
}

// ListDeadAccounts 列出所有失效账号（relogin 或已过期），不改任何数据。
func (s *Store) ListDeadAccounts(ctx context.Context, nowMS int64) ([]DeadAccount, error) {
	accounts, err := s.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]DeadAccount, 0)
	for _, a := range accounts {
		reason := a.DeadReason(nowMS)
		if reason == "" {
			continue
		}
		out = append(out, DeadAccount{
			ID: a.ID, Name: a.Name, UID: a.UID, Reason: reason,
			ExpiresAt: a.ExpiresAt, CoolReason: a.CoolReason,
		})
	}
	return out, nil
}

// PurgeDeadAccounts 在一个事务里删除所有失效账号。
// ids 由调用方先通过 ListDeadAccounts 拿给用户确认，这里只删确认过的那批：
// 避免用户在「预览」和「确认」之间新增的号被顺手带走。
func (s *Store) PurgeDeadAccounts(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	deleted := 0
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if err := s.deleteAccountRows(ctx, tx, id); err != nil {
			return 0, err
		}
		deleted++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if deleted > 0 {
		s.invalidateAccounts()
	}
	return deleted, nil
}

func joinComma(items []string) string {
	out := ""
	for i, it := range items {
		if i > 0 {
			out += ", "
		}
		out += it
	}
	return out
}
