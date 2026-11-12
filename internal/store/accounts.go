package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	ID              string
	UID             string
	Name            string
	Note            string
	Token           string
	RefreshToken    string
	TokenExp        int64
	Credits         int64
	CreditsAt       int64
	Status          string
	CoolUntil       int64
	CoolReason      string
	ExpiresAt       int64
	DeviceID        string
	DeviceName      string
	OSVersion       string
	OfficeIdentity  string
	Platform        string
	ClientChannel   string
	Language        string
	Meta            map[string]any
	Priority        int64
	Enabled         bool
	LastCheckinDay  string
	CreatedAt       int64
	UpdatedAt       int64
	LastUsed        int64
}

const accountCols = `id, uid, name, note, token, refresh_token, token_exp, credits, credits_at,
	status, cool_until, cool_reason, expires_at, device_id, device_name, os_version,
	office_identity, platform, client_channel, language, meta, priority, enabled,
	last_checkin_day, created_at, updated_at, last_used`

// 写入用的字段补丁：nil 表示不改。
type AccountPatch struct {
	Name            *string
	Note            *string
	Token           *string
	RefreshToken    *string
	TokenExp        *int64
	Credits         *int64
	CreditsAt       *int64
	Status          *string
	CoolUntil       *int64
	CoolReason      *string
	ExpiresAt       *int64
	DeviceID        *string
	DeviceName      *string
	OSVersion       *string
	OfficeIdentity  *string
	Platform        *string
	ClientChannel   *string
	Language        *string
	Meta            map[string]any
	Priority        *int64
	Enabled         *bool
	LastCheckinDay  *string
	LastUsed        *int64
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

func (s *Store) ListAccounts(ctx context.Context) ([]*Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+accountCols+` FROM accounts ORDER BY created_at ASC`)
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
	row := s.db.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE id = ?`, id)
	a, err := s.scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

func (s *Store) GetByUID(ctx context.Context, uid string) (*Account, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE uid = ?`, uid)
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO accounts (`+accountCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.UID, a.Name, a.Note, s.cipher.Encrypt(a.Token), s.cipher.Encrypt(a.RefreshToken),
		a.TokenExp, a.Credits, a.CreditsAt, a.Status, a.CoolUntil, a.CoolReason, a.ExpiresAt,
		a.DeviceID, a.DeviceName, a.OSVersion, a.OfficeIdentity, a.Platform, a.ClientChannel,
		a.Language, string(meta), a.Priority, enabled, a.LastCheckinDay,
		a.CreatedAt, a.UpdatedAt, a.LastUsed)
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
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET `+joinComma(sets)+` WHERE id = ?`, args...)
	return err
}

func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM model_cooldowns WHERE account_id = ?`, id); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM checkin_history WHERE account_id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, id)
	return err
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
