package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"iptv-manager/internal/core"
	"iptv-manager/internal/provider"
)

// ProxyInUseError explains why a proxy cannot be deleted yet.
type ProxyInUseError struct{ Channels, Subscriptions, Providers int }

func (e *ProxyInUseError) Error() string {
	return fmt.Sprintf("该代理仍被 %d 个频道、%d 个订阅和 %d 个内置直播源使用，请先修改它们的代理设置", e.Channels, e.Subscriptions, e.Providers)
}

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readSettings(ctx context.Context, q rowQueryer) (core.Settings, error) {
	var settings core.Settings
	var data string
	if err := q.QueryRowContext(ctx, `SELECT payload FROM settings WHERE id=1`).Scan(&data); err != nil {
		return settings, err
	}
	if err := json.Unmarshal([]byte(data), &settings); err != nil {
		return settings, err
	}
	return completeSettings(settings), nil
}

func writeSettings(ctx context.Context, q executor, settings core.Settings) error {
	data, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `UPDATE settings SET payload=? WHERE id=1`, string(data))
	return err
}

// completeSettings guarantees non-nil collections and one entry per
// registered built-in provider. Entries for unknown providers are dropped.
func completeSettings(s core.Settings) core.Settings {
	if s.Proxies == nil {
		s.Proxies = []core.Proxy{}
	}
	providers := make(map[string]string)
	for _, c := range provider.Catalogs() {
		ref := s.ProviderProxies[c.ID]
		if ref == "" {
			ref = core.DirectProxy
		}
		providers[c.ID] = ref
	}
	s.ProviderProxies = providers
	return s
}

func checkProxyRef(settings core.Settings, ref string) error {
	if ref == core.DirectProxy {
		return nil
	}
	if _, ok := settings.FindProxy(ref); !ok {
		return invalid("所选代理不存在，请刷新页面后重试")
	}
	return nil
}

func (s *Store) SaveProxy(ctx context.Context, p core.Proxy) (core.Proxy, error) {
	p, err := normalizeProxy(p)
	if err != nil {
		return p, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return p, err
	}
	defer tx.Rollback()
	settings, err := readSettings(ctx, tx)
	if err != nil {
		return p, err
	}
	if p.ID == "" {
		if len(settings.Proxies) >= maxProxies {
			return p, invalid("最多保存 %d 个代理", maxProxies)
		}
		p.ID, err = randomHex(12)
		if err != nil {
			return p, err
		}
		settings.Proxies = append(settings.Proxies, p)
	} else {
		found := false
		for i := range settings.Proxies {
			if settings.Proxies[i].ID == p.ID {
				settings.Proxies[i] = p
				found = true
			}
		}
		if !found {
			return p, ErrNotFound
		}
	}
	settings, err = normalizeSettings(settings)
	if err != nil {
		return p, err
	}
	if err := writeSettings(ctx, tx, settings); err != nil {
		return p, err
	}
	return p, tx.Commit()
}

// DeleteProxy refuses while any channel, subscription or built-in provider
// still references the proxy, so no connection silently changes its egress.
func (s *Store) DeleteProxy(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	settings, err := readSettings(ctx, tx)
	if err != nil {
		return err
	}
	if _, ok := settings.FindProxy(id); !ok {
		return ErrNotFound
	}
	usage := &ProxyInUseError{}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM channels WHERE proxy=? AND source_type<>'builtin'`, id).Scan(&usage.Channels); err != nil {
		return err
	}
	subs, err := subscriptions(ctx, tx)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if sub.Proxy == id {
			usage.Subscriptions++
		}
	}
	for _, ref := range settings.ProviderProxies {
		if ref == id {
			usage.Providers++
		}
	}
	if usage.Channels+usage.Subscriptions+usage.Providers > 0 {
		return usage
	}
	kept := make([]core.Proxy, 0, len(settings.Proxies))
	for _, p := range settings.Proxies {
		if p.ID != id {
			kept = append(kept, p)
		}
	}
	settings.Proxies = kept
	if err := writeSettings(ctx, tx, settings); err != nil {
		return err
	}
	return tx.Commit()
}

// SetProviderProxy changes the proxy shared by every channel of a built-in source.
func (s *Store) SetProviderProxy(ctx context.Context, providerID, raw string) error {
	if _, ok := provider.Lookup(providerID); !ok {
		return ErrNotFound
	}
	ref, err := proxyRef(raw)
	if err != nil {
		return err
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
	if err := checkProxyRef(settings, ref); err != nil {
		return err
	}
	settings.ProviderProxies[providerID] = ref
	if err := writeSettings(ctx, tx, settings); err != nil {
		return err
	}
	return tx.Commit()
}
