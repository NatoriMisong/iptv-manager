package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	"iptv-manager/internal/core"
	"iptv-manager/internal/source"
)

var ErrSubscriptionChanged = errors.New("订阅配置已变化，请重新同步")

const (
	maxSubscriptions       = 20
	maxSubscriptionSources = 10
	maxOriginDefaults      = 100
)

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

var domainPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// normalizeDefaults validates per-domain playback defaults and drops duplicates.
// Domains must already be in registrable form (demo.org, not a.demo.org).
func normalizeDefaults(defaults []core.DomainDefault) ([]core.DomainDefault, error) {
	if len(defaults) > maxOriginDefaults {
		return nil, invalid("域名规则过多")
	}
	result := make([]core.DomainDefault, 0, len(defaults))
	seen := map[string]bool{}
	for _, d := range defaults {
		domain := strings.ToLower(strings.TrimSpace(d.Domain))
		if domain == "" || len(domain) > 253 || domain != core.SiteDomain(domain) || seen[domain] || (net.ParseIP(domain) == nil && !domainPattern.MatchString(domain)) {
			return nil, invalid("域名规则必须为唯一的注册域名或 IP，例如 demo.org")
		}
		if d.Mode == "" {
			d.Mode = "relay"
		}
		if d.Mode != "relay" && d.Mode != "direct" {
			return nil, invalid("播放方式必须为中继或直连")
		}
		proxy, err := proxyRef(d.Proxy)
		if err != nil {
			return nil, err
		}
		seen[domain] = true
		result = append(result, core.DomainDefault{Domain: domain, Mode: d.Mode, Proxy: proxy})
	}
	return result, nil
}

// normalizeSubscription validates a subscription. Source IDs and sync status
// are carried over from old for every URL that is unchanged.
func normalizeSubscription(sub core.Subscription, old *core.Subscription) (core.Subscription, error) {
	if sub.ID != "" && !channelIDPattern.MatchString(sub.ID) {
		return sub, invalid("订阅 ID 无效")
	}
	if !safeText(sub.Name, 150) || strings.TrimSpace(sub.Name) == "" {
		return sub, invalid("订阅名称不能为空、过长或包含控制字符和双引号")
	}
	sub.Name = strings.TrimSpace(sub.Name)
	if len(sub.Sources) == 0 {
		return sub, invalid("至少填写一个 M3U 列表地址")
	}
	if len(sub.Sources) > maxSubscriptionSources {
		return sub, invalid("一个订阅最多包含 10 个列表地址")
	}
	previous := map[string]core.SubscriptionSource{}
	if old != nil {
		for _, src := range old.Sources {
			previous[src.URL] = src
		}
	}
	sources := make([]core.SubscriptionSource, 0, len(sub.Sources))
	seen := map[string]bool{}
	for _, src := range sub.Sources {
		raw := strings.TrimSpace(src.URL)
		if err := source.ValidateURL(raw); err != nil {
			return sub, invalid("%s", err)
		}
		if seen[raw] {
			return sub, invalid("列表地址重复")
		}
		seen[raw] = true
		kept, ok := previous[raw]
		if ok {
			sources = append(sources, kept)
			continue
		}
		id := src.ID
		if id == "" || !channelIDPattern.MatchString(id) {
			var err error
			if id, err = randomHex(8); err != nil {
				return sub, err
			}
		}
		sources = append(sources, core.SubscriptionSource{ID: id, URL: raw})
	}
	ids := map[string]bool{}
	for _, src := range sources {
		if ids[src.ID] {
			return sub, invalid("列表来源标识重复")
		}
		ids[src.ID] = true
	}
	sub.Sources = sources
	if sub.IntervalMinutes < 5 || sub.IntervalMinutes > 10080 {
		return sub, invalid("更新间隔须为 5–10080 分钟")
	}
	var err error
	if sub.Proxy, err = proxyRef(sub.Proxy); err != nil {
		return sub, err
	}
	if sub.Defaults, err = normalizeDefaults(sub.Defaults); err != nil {
		return sub, err
	}
	return sub, nil
}

func checkSubscriptionProxies(settings core.Settings, sub core.Subscription) error {
	if err := checkProxyRef(settings, sub.Proxy); err != nil {
		return err
	}
	for _, d := range sub.Defaults {
		if err := checkProxyRef(settings, d.Proxy); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SaveSubscription(ctx context.Context, sub core.Subscription) (core.Subscription, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return sub, err
	}
	defer tx.Rollback()
	settings, err := readSettings(ctx, tx)
	if err != nil {
		return sub, err
	}
	var old *core.Subscription
	if sub.ID != "" {
		current, err := scanSubscription(tx.QueryRowContext(ctx, `SELECT payload FROM subscriptions WHERE id=?`, sub.ID))
		if err != nil {
			return sub, err
		}
		old = &current
	}
	sub, err = normalizeSubscription(sub, old)
	if err != nil {
		return sub, err
	}
	if err := checkSubscriptionProxies(settings, sub); err != nil {
		return sub, err
	}
	if old == nil {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM subscriptions`).Scan(&count); err != nil {
			return sub, err
		}
		if count >= maxSubscriptions {
			return sub, invalid("最多保存 20 个订阅")
		}
		sub.ID, err = randomHex(12)
		if err != nil {
			return sub, err
		}
		sub.LastSync, sub.LastError = time.Time{}, ""
	} else {
		sub.LastSync, sub.LastError = old.LastSync, old.LastError
		// Lists that were removed take their catalogue and channels with them.
		for _, src := range old.Sources {
			if !sub.HasSource(src.ID) {
				if _, err := tx.ExecContext(ctx, `DELETE FROM subscription_entries WHERE subscription_id=? AND source_id=?`, sub.ID, src.ID); err != nil {
					return sub, err
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM channels WHERE subscription_id=? AND source_id=?`, sub.ID, src.ID); err != nil {
					return sub, err
				}
			}
		}
	}
	sub.LastAttempt = time.Time{}
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
	for _, stmt := range []string{`DELETE FROM channels WHERE subscription_id=?`, `DELETE FROM subscription_entries WHERE subscription_id=?`} {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			return err
		}
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

// ClearSubscription removes every channel created from the subscription but
// keeps the subscription and its catalogue. Returns the removed channel IDs.
func (s *Store) ClearSubscription(ctx context.Context, id string) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := scanSubscription(tx.QueryRowContext(ctx, `SELECT payload FROM subscriptions WHERE id=?`, id)); err != nil {
		return nil, err
	}
	ids, err := channelIDsWhere(ctx, tx, `subscription_id=?`, id)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM channels WHERE subscription_id=?`, id); err != nil {
		return nil, err
	}
	return ids, tx.Commit()
}

func channelIDsWhere(ctx context.Context, q queryer, where string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM channels WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// lockedSubscription loads the subscription inside tx and rejects stale callers.
func lockedSubscription(ctx context.Context, tx *sql.Tx, sub core.Subscription) (core.Subscription, error) {
	current, err := scanSubscription(tx.QueryRowContext(ctx, `SELECT payload FROM subscriptions WHERE id=?`, sub.ID))
	if err != nil {
		return current, err
	}
	if current.Revision != sub.Revision {
		return current, ErrSubscriptionChanged
	}
	return current, nil
}

// SubscriptionAttempt marks the start of a sync run (or records a run-level error).
func (s *Store) SubscriptionAttempt(ctx context.Context, sub core.Subscription, message string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := lockedSubscription(ctx, tx, sub)
	if err != nil {
		return err
	}
	current.LastAttempt = time.Now().UTC()
	current.LastError = message
	if err := writeSubscription(ctx, tx, current); err != nil {
		return err
	}
	return tx.Commit()
}

// SubscriptionResult records the outcome of a run: an empty message means every
// list succeeded and advances LastSync.
func (s *Store) SubscriptionResult(ctx context.Context, sub core.Subscription, message string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := lockedSubscription(ctx, tx, sub)
	if err != nil {
		return err
	}
	current.LastError = message
	if message == "" {
		current.LastSync = time.Now().UTC()
	}
	if err := writeSubscription(ctx, tx, current); err != nil {
		return err
	}
	return tx.Commit()
}

// SourceFailed stores a per-list error without touching its catalogue.
func (s *Store) SourceFailed(ctx context.Context, sub core.Subscription, sourceID, message string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := lockedSubscription(ctx, tx, sub)
	if err != nil {
		return err
	}
	found := false
	for i := range current.Sources {
		if current.Sources[i].ID == sourceID {
			current.Sources[i].LastError = message
			found = true
		}
	}
	if !found {
		return ErrSubscriptionChanged
	}
	if err := writeSubscription(ctx, tx, current); err != nil {
		return err
	}
	return tx.Commit()
}

// ApplySource replaces the catalogue of one list and refreshes the channels the
// user picked from it, matched by name. Channels whose name left the list are
// marked missing; reappearing names recover their ID and settings. Returns the
// IDs of channels whose playback data changed.
func (s *Store) ApplySource(ctx context.Context, sub core.Subscription, sourceID string, list source.Playlist) ([]string, error) {
	if len(list.Entries) == 0 || len(list.Entries) > source.MaxPlaylistChannels {
		return nil, invalid("列表没有可用频道或数量超出上限")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	current, err := lockedSubscription(ctx, tx, sub)
	if err != nil {
		return nil, err
	}
	index := -1
	for i := range current.Sources {
		if current.Sources[i].ID == sourceID {
			index = i
		}
	}
	if index < 0 {
		return nil, ErrSubscriptionChanged
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM subscription_entries WHERE subscription_id=? AND source_id=?`, sub.ID, sourceID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+channelFields+` FROM channels WHERE subscription_id=? AND source_id=?`, sub.ID, sourceID)
	if err != nil {
		return nil, err
	}
	picked := map[string]core.Channel{}
	for rows.Next() {
		ch, err := scanChannel(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		picked[ch.SourceKey] = ch
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	changed := make([]string, 0)
	seen := map[string]bool{}
	for i, entry := range list.Entries {
		if entry.Name == "" || seen[entry.Name] {
			return nil, invalid("列表中存在空白或重复的频道名称")
		}
		seen[entry.Name] = true
		if _, err := tx.ExecContext(ctx, `INSERT INTO subscription_entries(subscription_id,source_id,name,url,logo,group_name,position) VALUES(?,?,?,?,?,?,?)`, sub.ID, sourceID, entry.Name, entry.URL, entry.Logo, entry.Group, i); err != nil {
			return nil, err
		}
		ch, ok := picked[entry.Name]
		if !ok {
			continue
		}
		if ch.URL != entry.URL || ch.Logo != entry.Logo || ch.SourceGroup != entry.Group || ch.SourceMissing {
			probe := ch
			probe.URL, probe.Logo = entry.URL, entry.Logo
			if _, err := normalizeChannel(probe); err != nil {
				return nil, invalid("列表中存在无效的频道地址")
			}
			if _, err := tx.ExecContext(ctx, `UPDATE channels SET url=?,logo=?,source_group=?,source_missing=0 WHERE id=?`, entry.URL, entry.Logo, entry.Group, ch.ID); err != nil {
				return nil, err
			}
			changed = append(changed, ch.ID)
		}
	}
	for name, ch := range picked {
		if !seen[name] && !ch.SourceMissing {
			if _, err := tx.ExecContext(ctx, `UPDATE channels SET source_missing=1 WHERE id=?`, ch.ID); err != nil {
				return nil, err
			}
			changed = append(changed, ch.ID)
		}
	}
	current.Sources[index].LastSync = time.Now().UTC()
	current.Sources[index].LastError = ""
	current.Sources[index].Entries = len(list.Entries)
	current.Sources[index].Skipped = list.Skipped
	if err := writeSubscription(ctx, tx, current); err != nil {
		return nil, err
	}
	return changed, tx.Commit()
}

// SubscriptionEntries lists the catalogue in list order with the channel each
// entry has already produced, if any.
func (s *Store) SubscriptionEntries(ctx context.Context, id string) ([]core.SubscriptionEntry, error) {
	if _, err := s.Subscription(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.source_id,e.name,e.url,e.logo,e.group_name,e.position,COALESCE(c.id,'') FROM subscription_entries e LEFT JOIN channels c ON c.subscription_id=e.subscription_id AND c.source_id=e.source_id AND c.source_key=e.name WHERE e.subscription_id=? ORDER BY e.source_id,e.position`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]core.SubscriptionEntry, 0)
	for rows.Next() {
		var e core.SubscriptionEntry
		if err := rows.Scan(&e.SourceID, &e.Name, &e.URL, &e.Logo, &e.Group, &e.Position, &e.ChannelID); err != nil {
			return nil, err
		}
		e.Added = e.ChannelID != ""
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// AddSubscriptionChannels creates channels from catalogue entries in one
// transaction. Entries already in the channel list are skipped; unknown
// entries fail individually. The per-origin defaults are saved for next time.
func (s *Store) AddSubscriptionChannels(ctx context.Context, id string, req core.SubscriptionAddRequest) (core.BulkChannelResult, error) {
	var result core.BulkChannelResult
	if len(req.Items) == 0 {
		return result, invalid("请先选择频道")
	}
	if len(req.Items) > 1000 {
		return result, invalid("一次最多添加 1000 个频道")
	}
	if !safeText(req.Group, 150) {
		return result, invalid("分组包含不支持的字符或过长")
	}
	req.Group = strings.TrimSpace(req.Group)
	defaults, err := normalizeDefaults(req.Defaults)
	if err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	sub, err := scanSubscription(tx.QueryRowContext(ctx, `SELECT payload FROM subscriptions WHERE id=?`, id))
	if err != nil {
		return result, err
	}
	settings, err := readSettings(ctx, tx)
	if err != nil {
		return result, err
	}
	for _, d := range defaults {
		if err := checkProxyRef(settings, d.Proxy); err != nil {
			return result, err
		}
	}
	byDomain := map[string]core.DomainDefault{}
	for _, d := range defaults {
		byDomain[d.Domain] = d
	}
	var next int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sort_order),-1)+1 FROM channels`).Scan(&next); err != nil {
		return result, err
	}
	seen := map[string]bool{}
	added := 0
	for i, item := range req.Items {
		key := item.SourceID + "\x00" + item.Name
		if !sub.HasSource(item.SourceID) || item.Name == "" || seen[key] {
			return result, invalid("频道选择包含无效或重复的条目")
		}
		seen[key] = true
		line := core.BulkChannelItem{Line: i + 1, Name: item.Name}
		var entry core.SubscriptionEntry
		err := tx.QueryRowContext(ctx, `SELECT url,logo,group_name FROM subscription_entries WHERE subscription_id=? AND source_id=? AND name=?`, id, item.SourceID, item.Name).Scan(&entry.URL, &entry.Logo, &entry.Group)
		if errors.Is(err, sql.ErrNoRows) {
			line.Status, line.Message = "failed", "列表中已没有这个频道，请先同步"
			result.Failed++
			result.Results = append(result.Results, line)
			continue
		}
		if err != nil {
			return result, err
		}
		line.URL = entry.URL
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT id FROM channels WHERE subscription_id=? AND source_id=? AND source_key=?`, id, item.SourceID, item.Name).Scan(&existing)
		if err == nil {
			line.Status, line.Message, line.ChannelID = "skipped", "已在频道列表中", existing
			result.Skipped++
			result.Results = append(result.Results, line)
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return result, err
		}
		ch := core.Channel{Name: item.Name, URL: entry.URL, Logo: entry.Logo, Group: entry.Group, SourceType: "stream", SubscriptionID: id, SourceID: item.SourceID, SourceKey: item.Name, SourceGroup: entry.Group, Enabled: true, Mode: "relay", Proxy: core.DirectProxy, SortOrder: next}
		if req.Group != "" {
			ch.Group = req.Group
		}
		if d, ok := byDomain[core.URLDomain(entry.URL)]; ok {
			ch.Mode, ch.Proxy = d.Mode, d.Proxy
		}
		ch, err = normalizeChannel(ch)
		if err != nil {
			line.Status, line.Message = "failed", "频道地址或名称无效"
			result.Failed++
			result.Results = append(result.Results, line)
			continue
		}
		if err := checkChannelCapacity(ctx, tx, 1); err != nil {
			return result, err
		}
		if ch.ID, err = randomHex(12); err != nil {
			return result, err
		}
		if err := insertChannel(ctx, tx, ch); err != nil {
			return result, err
		}
		next++
		added++
		line.Status, line.Message, line.ChannelID = "added", "已添加", ch.ID
		result.Added++
		result.Results = append(result.Results, line)
	}
	if len(defaults) > 0 {
		merged := map[string]core.DomainDefault{}
		for _, d := range sub.Defaults {
			merged[d.Domain] = d
		}
		for _, d := range defaults {
			merged[d.Domain] = d
		}
		sub.Defaults = sub.Defaults[:0]
		for _, d := range merged {
			sub.Defaults = append(sub.Defaults, d)
		}
		sort.Slice(sub.Defaults, func(i, j int) bool { return sub.Defaults[i].Domain < sub.Defaults[j].Domain })
		if len(sub.Defaults) > maxOriginDefaults {
			return result, invalid("域名规则过多")
		}
		if err := writeSubscription(ctx, tx, sub); err != nil {
			return result, err
		}
	}
	return result, tx.Commit()
}
