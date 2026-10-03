package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"iptv-manager/internal/core"
	"iptv-manager/internal/source"
)

var ErrSubscriptionChanged = errors.New("订阅配置已变化，请重新同步")

func checkChannelCapacity(ctx context.Context, tx *sql.Tx, additional int) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM channels`).Scan(&count); err != nil {
		return err
	}
	if count+additional > 1000 {
		return invalid("频道总数最多为 1000，请先移除不再需要的频道或订阅")
	}
	return nil
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}
type executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func subscriptions(ctx context.Context, q queryer) ([]core.Subscription, error) {
	rows, err := q.QueryContext(ctx, `SELECT payload FROM subscriptions ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]core.Subscription, 0)
	for rows.Next() {
		var payload string
		var sub core.Subscription
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payload), &sub); err != nil {
			return nil, err
		}
		result = append(result, sub)
	}
	return result, rows.Err()
}
func (s *Store) Subscriptions(ctx context.Context) ([]core.Subscription, error) {
	return subscriptions(ctx, s.db)
}
func scanSubscription(row scanner) (core.Subscription, error) {
	var payload string
	var sub core.Subscription
	if err := row.Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return sub, err
	}
	err := json.Unmarshal([]byte(payload), &sub)
	return sub, err
}
func (s *Store) Subscription(ctx context.Context, id string) (core.Subscription, error) {
	return scanSubscription(s.db.QueryRowContext(ctx, `SELECT payload FROM subscriptions WHERE id=?`, id))
}
func writeSubscription(ctx context.Context, q executor, sub core.Subscription) error {
	payload, err := json.Marshal(sub)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO subscriptions(id,payload) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload`, sub.ID, string(payload))
	return err
}
func normalizeSubscription(sub core.Subscription) (core.Subscription, error) {
	if sub.ID != "" && !channelIDPattern.MatchString(sub.ID) {
		return sub, invalid("订阅 ID 无效")
	}
	if !safeText(sub.Name, 150) || strings.TrimSpace(sub.Name) == "" {
		return sub, invalid("订阅名称不能为空、过长或包含控制字符和双引号")
	}
	sub.Name = strings.TrimSpace(sub.Name)
	if err := source.ValidateURL(sub.URL); err != nil {
		return sub, invalid("%s", err)
	}
	if sub.IntervalMinutes < 5 || sub.IntervalMinutes > 10080 {
		return sub, invalid("更新间隔须为 5–10080 分钟")
	}
	var err error
	sub.Proxy, err = proxyRef(sub.Proxy)
	return sub, err
}
func (s *Store) SaveSubscription(ctx context.Context, sub core.Subscription) (core.Subscription, error) {
	sub, err := normalizeSubscription(sub)
	if err != nil {
		return sub, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return sub, err
	}
	defer tx.Rollback()
	settings, err := readSettings(ctx, tx)
	if err != nil {
		return sub, err
	}
	if err := checkProxyRef(settings, sub.Proxy); err != nil {
		return sub, err
	}
	if sub.ID == "" {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM subscriptions`).Scan(&count); err != nil {
			return sub, err
		}
		if count >= 20 {
			return sub, invalid("最多保存 20 个订阅")
		}
		sub.ID, err = randomHex(12)
		if err != nil {
			return sub, err
		}
		sub.LastAttempt, sub.LastSync, sub.LastError, sub.Skipped = time.Time{}, time.Time{}, "", 0
	} else {
		old, err := scanSubscription(tx.QueryRowContext(ctx, `SELECT payload FROM subscriptions WHERE id=?`, sub.ID))
		if err != nil {
			return sub, err
		}
		sub.LastAttempt, sub.LastSync, sub.LastError, sub.Skipped = time.Time{}, old.LastSync, old.LastError, old.Skipped
	}
	sub.Revision = int(time.Now().UnixNano())
	if err := writeSubscription(ctx, tx, sub); err != nil {
		return sub, err
	}
	return sub, tx.Commit()
}
func (s *Store) DeleteSubscription(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM channels WHERE subscription_id=?`, id); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM subscriptions WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}
func (s *Store) SubscriptionAttempt(ctx context.Context, sub core.Subscription, message string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := scanSubscription(tx.QueryRowContext(ctx, `SELECT payload FROM subscriptions WHERE id=?`, sub.ID))
	if err != nil {
		return err
	}
	if current.Revision != sub.Revision {
		return ErrSubscriptionChanged
	}
	current.LastAttempt = time.Now().UTC()
	current.LastError = message
	if err := writeSubscription(ctx, tx, current); err != nil {
		return err
	}
	return tx.Commit()
}

// ApplySubscription preserves local playback settings and permanent channel IDs.
// Missing sources are marked unavailable so reappearing entries reuse their ID.
func (s *Store) ApplySubscription(ctx context.Context, sub core.Subscription, list source.Playlist) ([]string, error) {
	if len(list.Entries) == 0 || len(list.Entries) > source.MaxPlaylistChannels {
		return nil, invalid("订阅频道数量无效")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	current, err := scanSubscription(tx.QueryRowContext(ctx, `SELECT payload FROM subscriptions WHERE id=?`, sub.ID))
	if err != nil {
		return nil, err
	}
	if current.Revision != sub.Revision {
		return nil, ErrSubscriptionChanged
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+channelFields+` FROM channels WHERE subscription_id=?`, sub.ID)
	if err != nil {
		return nil, err
	}
	old := map[string]core.Channel{}
	for rows.Next() {
		ch, err := scanChannel(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		old[ch.SourceKey] = ch
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var next int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sort_order),-1)+1 FROM channels`).Scan(&next); err != nil {
		return nil, err
	}
	changed := make([]string, 0)
	seen := map[string]bool{}
	for _, entry := range list.Entries {
		if entry.Key == "" || seen[entry.Key] {
			return nil, invalid("订阅频道身份重复或缺失")
		}
		seen[entry.Key] = true
		ch, exists := old[entry.Key]
		if !exists {
			ch = core.Channel{SubscriptionID: sub.ID, SourceKey: entry.Key, Enabled: true, Mode: "inherit", Proxy: core.DirectProxy, SortOrder: next}
			next++
			ch.ID, err = randomHex(12)
			if err != nil {
				return nil, err
			}
		}
		ch.SourceType, ch.Name, ch.URL, ch.Group, ch.Logo, ch.SourceMissing = "stream", entry.Name, entry.URL, entry.Group, entry.Logo, false
		ch, err = normalizeChannel(ch)
		if err != nil {
			return nil, invalid("订阅中存在无效的频道配置")
		}
		if !exists {
			if err := checkChannelCapacity(ctx, tx, 1); err != nil {
				return nil, err
			}
			if err := insertChannel(ctx, tx, ch); err != nil {
				return nil, err
			}
			changed = append(changed, ch.ID)
		} else if ch != old[entry.Key] {
			if _, err := tx.ExecContext(ctx, `UPDATE channels SET name=?,url=?,group_name=?,logo=?,source_missing=0 WHERE id=?`, ch.Name, ch.URL, ch.Group, ch.Logo, ch.ID); err != nil {
				return nil, err
			}
			changed = append(changed, ch.ID)
		}
	}
	for key, ch := range old {
		if !seen[key] && !ch.SourceMissing {
			if _, err := tx.ExecContext(ctx, `UPDATE channels SET source_missing=1 WHERE id=?`, ch.ID); err != nil {
				return nil, err
			}
			changed = append(changed, ch.ID)
		}
	}
	current.LastSync = time.Now().UTC()
	current.LastAttempt = current.LastSync
	current.LastError = ""
	current.Skipped = list.Skipped
	if err := writeSubscription(ctx, tx, current); err != nil {
		return nil, err
	}
	return changed, tx.Commit()
}
