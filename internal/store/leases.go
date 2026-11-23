package store

import (
	"context"
	"time"
)

// 多槽位在途租约。
//
// 早期实现只在 accounts 上放一个 lease_until：整号要么被一个请求独占，要么
// 完全放开。现在改成「一个账号 N 个槽位」：同一账号最多同时被 N 个请求占用，
// 等于把单号并发上限做成可配（面板「租约并发数」）。多副本下靠
// account_lease_slots 的主键 (account_id, slot) 与 UPDATE 的 CAS 抢占，
// 不使用进程内计数——进程内 map 在多副本下判断不了「别的副本占没占」。
//
// 所有槽位的到期时刻都由 lease_until 汇总回 accounts，选号层照旧只看这一个
// 字段（> now 表示至少还有一个槽位被占），不用为每个新列改选号条件。

// AcquireLeaseSlot 抢占账号的一个空槽位。
//
// 返回 (slot, until, ok)：
//   - ok=true 时 slot 是要记住的槽号（释放与续期都要带上），until 是到期时刻；
//   - ok=false 表示所有槽位都被别人占着（或抢的瞬间被别的副本拿走）。
//
// 已是空的到期槽可直接复用。槽位按序号从小到大扫：并发时大家都先抢 0 号槽，
// 抢不过再退到 1 号，冲突面小、不会把 N 个槽位均匀摊开造成「每次都要扫全表」。
func (s *Store) AcquireLeaseSlot(ctx context.Context, id string, slots int, ttlMS, now int64) (int, int64, bool, error) {
	if slots <= 0 {
		slots = 1
	}
	for slot := 0; slot < slots; slot++ {
		until := now + ttlMS
		// 先确保这一行存在（已存在则什么也不做）。不依赖 read-then-write：
		// 多副本同时插入同一主键时，一方失败、另一方成功，都是预期结果。
		if _, err := s.exec(ctx,
			`INSERT INTO account_lease_slots (account_id, slot, slot_until) VALUES (?,?,0)
			 ON CONFLICT (account_id, slot) DO NOTHING`, id, slot); err != nil {
			return 0, 0, false, err
		}
		res, err := s.exec(ctx,
			`UPDATE account_lease_slots SET slot_until = ?
			 WHERE account_id = ? AND slot = ? AND slot_until <= ?`,
			until, id, slot, now)
		if err != nil {
			return 0, 0, false, err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			continue // 这一槽已被别人占着，试下一个
		}
		s.refreshLeaseUntil(ctx, id)
		return slot, until, true, nil
	}
	return 0, 0, false, nil
}

// ReleaseLeaseSlot 释放指定槽位。until 做 CAS：只有槽位还停在自己写进去的
// 到期时刻才清零，避免 TTL 过期后被别的副本接管、自己又把它放掉。
func (s *Store) ReleaseLeaseSlot(ctx context.Context, id string, slot int, until int64) error {
	var err error
	if until > 0 {
		_, err = s.exec(ctx,
			`UPDATE account_lease_slots SET slot_until = 0
			 WHERE account_id = ? AND slot = ? AND slot_until = ?`, id, slot, until)
	} else {
		_, err = s.exec(ctx,
			`UPDATE account_lease_slots SET slot_until = 0 WHERE account_id = ? AND slot = ?`, id, slot)
	}
	s.refreshLeaseUntil(ctx, id)
	return err
}

// RenewLeaseSlot 续期指定槽位，expect 是当初抢到时拿到的到期时刻。
func (s *Store) RenewLeaseSlot(ctx context.Context, id string, slot int, ttlMS, expect, now int64) (int64, error) {
	until := now + ttlMS
	res, err := s.exec(ctx,
		`UPDATE account_lease_slots SET slot_until = ?
		 WHERE account_id = ? AND slot = ? AND slot_until = ?`, until, id, slot, expect)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, nil
	}
	s.refreshLeaseUntil(ctx, id)
	return until, nil
}

// ClearLeaseSlots 强清某账号的全部槽位（面板手动解除残租约用）。
func (s *Store) ClearLeaseSlots(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM account_lease_slots WHERE account_id = ?`, id)
	s.refreshLeaseUntil(ctx, id)
	return err
}

// LeaseSlotStats 返回账号当前被占的槽位数与其中最晚的到期时刻。
// 只用于面板展示与选号层的汇总，不做抢占判断。
func (s *Store) LeaseSlotStats(ctx context.Context, id string, now int64) (count int, latest int64, err error) {
	row := s.queryRow(ctx,
		`SELECT COUNT(*), COALESCE(MAX(slot_until), 0) FROM account_lease_slots
		 WHERE account_id = ? AND slot_until > ?`, id, now)
	err = row.Scan(&count, &latest)
	return
}

// LeaseCounts 一次读出所有账号当前被占的槽位数。
//
// 选号层用它判断「这个号的并发槽位满了没有」。必须是库里的事实而不是
// 本进程的计数：槽位可能被别的副本占着，进程内 map 看不见。
// 一条聚合查询代替逐号 COUNT，换号重试时也不会变成 N 次往返。
func (s *Store) LeaseCounts(ctx context.Context, now int64) (map[string]int, error) {
	rows, err := s.query(ctx,
		`SELECT account_id, COUNT(*) FROM account_lease_slots
		 WHERE slot_until > ? GROUP BY account_id`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// HydrateLeaseSlots 在进程启动时把库里的占用汇总回 accounts.lease_until。
// 老库升级到多槽位后，这一列要能和槽位表对上，否则选号层会以为号是空的。
func (s *Store) HydrateLeaseSlots(ctx context.Context) error {
	now := time.Now().UnixMilli()
	rows, err := s.query(ctx,
		`SELECT account_id, MAX(slot_until) FROM account_lease_slots
		 WHERE slot_until > ? GROUP BY account_id`, now)
	if err != nil {
		return err
	}
	defer rows.Close()
	type row struct {
		id    string
		until int64
	}
	var batch []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.until); err != nil {
			return err
		}
		batch = append(batch, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range batch {
		if _, err := s.exec(ctx,
			`UPDATE accounts SET lease_until = ? WHERE id = ? AND lease_until < ?`,
			r.until, r.id, r.until); err != nil {
			return err
		}
	}
	s.invalidateAccounts()
	return nil
}

// refreshLeaseUntil 把槽位表里该账号最晚的到期时刻汇总回 accounts.lease_until。
//
// 选号与「下一次可能有号」的判断都只看这一个字段：任一槽位被占即 lease_until
// > now。汇总失败不影响抢占本身（槽位表才是权威），只是选号会短暂保守/激进。
func (s *Store) refreshLeaseUntil(ctx context.Context, id string) {
	now := time.Now().UnixMilli()
	var latest int64
	_ = s.queryRow(ctx,
		`SELECT COALESCE(MAX(slot_until), 0) FROM account_lease_slots WHERE account_id = ? AND slot_until > ?`,
		id, now).Scan(&latest)
	_, _ = s.exec(ctx, `UPDATE accounts SET lease_until = ? WHERE id = ?`, latest, id)
	s.invalidateAccounts()
}
