// Package store owns the durable configuration. Media never goes into SQLite.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"iptv-manager/internal/core"
	_ "modernc.org/sqlite"
)

var (
	ErrNotFound   = errors.New("not found")
	ErrValidation = errors.New("validation failed")
)

type Store struct{ db *sql.DB }

func randomHex(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func Open(path string) (*Store, error) {
	dsn := ":memory:"
	if path != ":memory:" {
		if path == "" {
			return nil, invalid("database path cannot be empty")
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
			return nil, err
		}
		file, err := os.OpenFile(absolute, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		if err := file.Chmod(0600); err != nil {
			file.Close()
			return nil, err
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
		dsn = (&url.URL{Scheme: "file", Path: absolute}).String()
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	for _, pragma := range []string{"PRAGMA busy_timeout = 5000", "PRAGMA journal_mode = WAL", "PRAGMA foreign_keys = ON", "PRAGMA synchronous = NORMAL"} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := s.initialize(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) initialize(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != 0 && version != 5 {
		return fmt.Errorf("unsupported database schema %d; IPTV Manager 0.4 requires schema version 5 or a new database", version)
	}
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS channels (id TEXT PRIMARY KEY, name TEXT NOT NULL, url TEXT NOT NULL, group_name TEXT NOT NULL, logo TEXT NOT NULL, enabled INTEGER NOT NULL, sort_order INTEGER NOT NULL, mode TEXT NOT NULL, quality INTEGER NOT NULL, proxy TEXT NOT NULL, source_type TEXT NOT NULL, subscription_id TEXT NOT NULL DEFAULT '', source_key TEXT NOT NULL DEFAULT '', source_missing INTEGER NOT NULL DEFAULT 0, provider_id TEXT NOT NULL DEFAULT '', provider_channel_id TEXT NOT NULL DEFAULT '')`,
		`CREATE INDEX IF NOT EXISTS channels_order ON channels(sort_order, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS channels_subscription_key ON channels(subscription_id,source_key) WHERE subscription_id<>''`,
		`CREATE TABLE IF NOT EXISTS settings (id INTEGER PRIMARY KEY CHECK (id = 1), payload TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS traffic (month TEXT PRIMARY KEY, bytes INTEGER NOT NULL CHECK (bytes >= 0))`,
		`CREATE TABLE IF NOT EXISTS subscriptions (id TEXT PRIMARY KEY, payload TEXT NOT NULL)`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings`).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		token, err := randomHex(32)
		if err != nil {
			return err
		}
		settings := completeSettings(core.Settings{DefaultQuality: 720, MonthlyBudgetGB: 800, PlaybackToken: token})
		data, err := json.Marshal(settings)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(id,payload) VALUES(1,?)`, string(data)); err != nil {
			return err
		}
		for i, source := range []struct{ name, id string }{{"中天新闻", "vr3XyVCR4T0"}, {"东森新闻 51", "V1p33hqPrUk"}} {
			id, err := randomHex(12)
			if err != nil {
				return err
			}
			ch := core.Channel{ID: id, Name: source.name, URL: "https://www.youtube.com/watch?v=" + source.id, SourceType: "youtube", Group: "新闻", Enabled: true, SortOrder: i, Mode: "relay", Proxy: core.DirectProxy}
			if err := insertChannel(ctx, tx, ch); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA user_version = 5`); err != nil {
		return err
	}
	return tx.Commit()
}

const channelFields = "id,name,url,group_name,logo,enabled,sort_order,mode,quality,proxy,source_type,subscription_id,source_key,source_missing,provider_id,provider_channel_id"

type scanner interface{ Scan(dest ...any) error }

func scanChannel(row scanner) (core.Channel, error) {
	var ch core.Channel
	err := row.Scan(&ch.ID, &ch.Name, &ch.URL, &ch.Group, &ch.Logo, &ch.Enabled, &ch.SortOrder, &ch.Mode, &ch.Quality, &ch.Proxy, &ch.SourceType, &ch.SubscriptionID, &ch.SourceKey, &ch.SourceMissing, &ch.ProviderID, &ch.ProviderChannelID)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return ch, err
}

func (s *Store) Channels(ctx context.Context) ([]core.Channel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+channelFields+` FROM channels ORDER BY sort_order,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	channels := make([]core.Channel, 0)
	for rows.Next() {
		ch, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		channels = append(channels, ch)
	}
	return channels, rows.Err()
}

func (s *Store) Channel(ctx context.Context, id string) (core.Channel, error) {
	return scanChannel(s.db.QueryRowContext(ctx, `SELECT `+channelFields+` FROM channels WHERE id=?`, id))
}

func insertChannel(ctx context.Context, tx *sql.Tx, ch core.Channel) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO channels (`+channelFields+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, ch.ID, ch.Name, ch.URL, ch.Group, ch.Logo, ch.Enabled, ch.SortOrder, ch.Mode, ch.Quality, ch.Proxy, ch.SourceType, ch.SubscriptionID, ch.SourceKey, ch.SourceMissing, ch.ProviderID, ch.ProviderChannelID)
	return err
}

func (s *Store) SaveChannel(ctx context.Context, ch core.Channel) (core.Channel, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ch, err
	}
	defer tx.Rollback()
	settings, err := readSettings(ctx, tx)
	if err != nil {
		return ch, err
	}
	if ch.ID == "" {
		ch, err = normalizeChannel(ch)
		if err != nil {
			return ch, err
		}
		if err := checkProxyRef(settings, ch.Proxy); err != nil {
			return ch, err
		}
		if err := checkChannelCapacity(ctx, tx, 1); err != nil {
			return ch, err
		}
		ch.SubscriptionID, ch.SourceKey, ch.SourceMissing = "", "", false
		ch.ID, err = randomHex(12)
		if err != nil {
			return ch, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sort_order),-1)+1 FROM channels`).Scan(&ch.SortOrder); err != nil {
			return ch, err
		}
		if err := insertChannel(ctx, tx, ch); err != nil {
			return ch, err
		}
	} else {
		ch, err = updateChannel(ctx, tx, settings, ch)
		if err != nil {
			return ch, err
		}
	}
	return ch, tx.Commit()
}

// updateChannel applies a single-channel edit inside tx: source fields owned by
// a subscription or provider are preserved, then the result is validated.
func updateChannel(ctx context.Context, tx *sql.Tx, settings core.Settings, ch core.Channel) (core.Channel, error) {
	current, err := scanChannel(tx.QueryRowContext(ctx, `SELECT `+channelFields+` FROM channels WHERE id=?`, ch.ID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ch, ErrNotFound
		}
		return ch, err
	}
	ch.SubscriptionID, ch.SourceKey, ch.SourceMissing, ch.SortOrder = current.SubscriptionID, current.SourceKey, current.SourceMissing, current.SortOrder
	if current.SubscriptionID != "" {
		ch.Name, ch.URL, ch.Group, ch.Logo, ch.SourceType = current.Name, current.URL, current.Group, current.Logo, current.SourceType
	}
	if current.IsBuiltin() || current.SubscriptionID != "" {
		ch.SourceType, ch.URL = current.SourceType, current.URL
		ch.ProviderID, ch.ProviderChannelID = current.ProviderID, current.ProviderChannelID
	}
	ch, err = normalizeChannel(ch)
	if err != nil {
		return ch, err
	}
	if err := checkProxyRef(settings, ch.Proxy); err != nil {
		return ch, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE channels SET name=?,url=?,group_name=?,logo=?,enabled=?,mode=?,quality=?,proxy=?,source_type=?,provider_id=?,provider_channel_id=? WHERE id=?`, ch.Name, ch.URL, ch.Group, ch.Logo, ch.Enabled, ch.Mode, ch.Quality, ch.Proxy, ch.SourceType, ch.ProviderID, ch.ProviderChannelID, ch.ID)
	if err != nil {
		return ch, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return ch, err
	}
	if rows == 0 {
		return ch, ErrNotFound
	}
	return ch, nil
}

// channelIDs validates a batch of IDs: non-empty, unique, well-formed and
// within the project-wide channel limit.
func channelIDs(ids []string) error {
	if len(ids) == 0 {
		return invalid("请先选择频道")
	}
	if len(ids) > 1000 {
		return invalid("一次最多处理 1000 个频道")
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !channelIDPattern.MatchString(id) || seen[id] {
			return invalid("频道选择包含无效或重复的 ID")
		}
		seen[id] = true
	}
	return nil
}

// UpdateChannels applies the same edit to every listed channel atomically.
// Any missing channel or invalid value aborts the whole batch.
func (s *Store) UpdateChannels(ctx context.Context, req core.BulkChannelUpdate) error {
	if err := channelIDs(req.IDs); err != nil {
		return err
	}
	if req.Group == nil && req.Mode == nil && req.Quality == nil && req.Proxy == nil && req.Enabled == nil {
		return invalid("请至少选择一项要修改的内容")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	settings, err := readSettings(ctx, tx)
	if err != nil {
		return err
	}
	for _, id := range req.IDs {
		ch, err := scanChannel(tx.QueryRowContext(ctx, `SELECT `+channelFields+` FROM channels WHERE id=?`, id))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if req.Group != nil {
			ch.Group = *req.Group
		}
		if req.Mode != nil {
			ch.Mode = *req.Mode
		}
		if req.Quality != nil {
			ch.Quality = *req.Quality
		}
		if req.Proxy != nil {
			ch.Proxy = *req.Proxy
		}
		if req.Enabled != nil {
			ch.Enabled = *req.Enabled
		}
		if _, err := updateChannel(ctx, tx, settings, ch); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteChannels removes the listed channels atomically. Subscription channels
// are refused because the next sync would recreate them; disable them instead.
func (s *Store) DeleteChannels(ctx context.Context, ids []string) error {
	if err := channelIDs(ids); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		var subscription string
		if err := tx.QueryRowContext(ctx, `SELECT subscription_id FROM channels WHERE id=?`, id).Scan(&subscription); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if subscription != "" {
			return invalid("订阅频道随自定义订阅同步，不能单独删除；可以停用频道，或在订阅页清理频道、删除订阅")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM channels WHERE id=?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteChannel(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM channels WHERE id=?`, id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// Reorder requires every channel exactly once and only changes sort_order.
func (s *Store) Reorder(ctx context.Context, ids []string) error {
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !channelIDPattern.MatchString(id) || seen[id] {
			return invalid("channel ordering contains an invalid or duplicate ID")
		}
		seen[id] = true
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM channels`).Scan(&count); err != nil {
		return err
	}
	if count != len(ids) {
		return invalid("channel ordering must contain every channel exactly once")
	}
	for i, id := range ids {
		result, err := tx.ExecContext(ctx, `UPDATE channels SET sort_order=? WHERE id=?`, i, id)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 0 {
			return invalid("channel ordering contains an unknown ID")
		}
	}
	return tx.Commit()
}

func (s *Store) Settings(ctx context.Context) (core.Settings, error) {
	return readSettings(ctx, s.db)
}

func (s *Store) SaveSettings(ctx context.Context, settings core.Settings) error {
	settings, err := normalizeSettings(settings)
	if err != nil {
		return err
	}
	return writeSettings(ctx, s.db, settings)
}

func (s *Store) AdminHash(ctx context.Context) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='admin_hash'`).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return hash, err
}

func (s *Store) SetAdminHash(ctx context.Context, hash string) error {
	if len(hash) < 20 || len(hash) > 1024 {
		return invalid("invalid administrator password hash")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES('admin_hash',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, hash)
	return err
}

func (s *Store) Export(ctx context.Context) (core.Backup, error) {
	// One read transaction gives a consistent backup during concurrent edits.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return core.Backup{}, err
	}
	defer tx.Rollback()
	b := core.Backup{Version: 5, Channels: make([]core.Channel, 0)}
	b.Settings, err = readSettings(ctx, tx)
	if err != nil {
		return b, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+channelFields+` FROM channels ORDER BY sort_order,id`)
	if err != nil {
		return b, err
	}
	for rows.Next() {
		ch, err := scanChannel(rows)
		if err != nil {
			rows.Close()
			return b, err
		}
		b.Channels = append(b.Channels, ch)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return b, err
	}
	if err := rows.Close(); err != nil {
		return b, err
	}
	b.Subscriptions, err = subscriptions(ctx, tx)
	if err != nil {
		return b, err
	}
	return b, tx.Commit()
}

func (s *Store) Import(ctx context.Context, b core.Backup) error {
	if b.Version != 5 {
		return invalid("unsupported backup version")
	}
	if len(b.Channels) > 1000 {
		return invalid("backup exceeds the 1000-channel limit")
	}
	if len(b.Subscriptions) > 20 {
		return invalid("too many subscriptions")
	}
	settings, err := normalizeSettings(b.Settings)
	if err != nil {
		return err
	}
	channels := make([]core.Channel, len(b.Channels))
	subs := make(map[string]core.Subscription)
	for _, sub := range b.Subscriptions {
		normalized, err := normalizeSubscription(sub)
		if err != nil || normalized.ID == "" {
			return invalid("invalid subscription")
		}
		if err := checkProxyRef(settings, normalized.Proxy); err != nil {
			return invalid("subscription references a missing proxy")
		}
		if _, exists := subs[sub.ID]; exists {
			return invalid("duplicate subscription ID")
		}
		subs[sub.ID] = normalized
	}
	keys := map[string]bool{}
	seen := make(map[string]bool, len(channels))
	orders := make(map[int]bool, len(channels))
	for i, source := range b.Channels {
		ch, err := normalizeChannel(source)
		if err != nil {
			return fmt.Errorf("channel %d: %w", i+1, err)
		}
		if err := checkProxyRef(settings, ch.Proxy); err != nil {
			return fmt.Errorf("channel %d: %w", i+1, err)
		}
		if ch.SubscriptionID != "" {
			if _, ok := subs[ch.SubscriptionID]; !ok || !ch.IsStream() || ch.SourceKey == "" || len(ch.SourceKey) > 8200 || keys[ch.SubscriptionID+"\x00"+ch.SourceKey] {
				return invalid("invalid subscription channel")
			}
			keys[ch.SubscriptionID+"\x00"+ch.SourceKey] = true
		} else {
			ch.SourceKey, ch.SourceMissing = "", false
		}
		if ch.ID == "" || seen[ch.ID] {
			return invalid("backup contains missing or duplicate channel IDs")
		}
		if orders[ch.SortOrder] {
			return invalid("backup contains duplicate channel sort orders")
		}
		seen[ch.ID], orders[ch.SortOrder] = true, true
		channels[i] = ch
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM channels`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM subscriptions`); err != nil {
		return err
	}
	for _, sub := range subs {
		sub.Revision = int(time.Now().UnixNano())
		if err := writeSubscription(ctx, tx, sub); err != nil {
			return err
		}
	}
	for _, ch := range channels {
		if err := insertChannel(ctx, tx, ch); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE settings SET payload=? WHERE id=1`, string(data)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AddTraffic(ctx context.Context, month string, bytes int64) error {
	if !monthPattern.MatchString(month) || bytes < 0 {
		return invalid("invalid traffic month or byte count")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO traffic(month,bytes) VALUES(?,?) ON CONFLICT(month) DO UPDATE SET bytes=bytes+excluded.bytes`, month, bytes)
	return err
}

func (s *Store) Traffic(ctx context.Context, month string) (core.Traffic, error) {
	result := core.Traffic{Month: month}
	if !monthPattern.MatchString(month) {
		return result, invalid("invalid traffic month")
	}
	err := s.db.QueryRowContext(ctx, `SELECT bytes FROM traffic WHERE month=?`, month).Scan(&result.Bytes)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	return result, err
}
