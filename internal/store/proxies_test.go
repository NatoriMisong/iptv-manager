package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"iptv-manager/internal/core"
)

func TestProxyLifecycleAndReferences(t *testing.T) {
	s := testStore(t)
	for _, bad := range []core.Proxy{
		{Name: "", Scheme: "http", Host: "proxy.example", Port: 8080},
		{Name: "x", Scheme: "ftp", Host: "proxy.example", Port: 21},
		{Name: "x", Scheme: "http", Host: "", Port: 8080},
		{Name: "x", Scheme: "http", Host: "proxy.example/path", Port: 8080},
		{Name: "x", Scheme: "http", Host: "user@proxy.example", Port: 8080},
		{Name: "x", Scheme: "http", Host: "proxy.example", Port: 0},
		{Name: "x", Scheme: "http", Host: "proxy.example", Port: 70000},
		{Name: "x", Scheme: "http", Host: "proxy.example", Port: 8080, Password: "line\nbreak"},
		{ID: "not-hex", Name: "x", Scheme: "http", Host: "proxy.example", Port: 8080},
	} {
		if _, err := s.SaveProxy(testContext, bad); !errors.Is(err, ErrValidation) {
			t.Errorf("accepted invalid proxy %+v: %v", bad, err)
		}
	}
	first, err := s.SaveProxy(testContext, core.Proxy{Name: " 家里 ", Scheme: "SOCKS5", Host: "10.10.1.38", Port: 1080})
	if err != nil || len(first.ID) != 24 || first.Name != "家里" || first.URL() != "socks5://10.10.1.38:1080" {
		t.Fatalf("first proxy = %+v, %v", first, err)
	}
	second, err := s.SaveProxy(testContext, core.Proxy{Name: "公司", Scheme: "http", Host: "Proxy.Example", Port: 8080, Username: "user", Password: "p@ss:word"})
	if err != nil || second.URL() != "http://user:p%40ss%3Aword@proxy.example:8080" || second.Address() != "http://proxy.example:8080" {
		t.Fatalf("second proxy = %+v, %v", second, err)
	}
	if _, err := s.SaveProxy(testContext, core.Proxy{Name: "家里", Scheme: "http", Host: "other.example", Port: 1}); !errors.Is(err, ErrValidation) {
		t.Fatalf("duplicate proxy name accepted: %v", err)
	}
	if _, err := s.SaveProxy(testContext, core.Proxy{ID: "ffffffffffffffffffffffff", Name: "x", Scheme: "http", Host: "h", Port: 1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown proxy updated: %v", err)
	}
	first.Port = 1081
	if updated, err := s.SaveProxy(testContext, first); err != nil || updated.ID != first.ID || updated.URL() != "socks5://10.10.1.38:1081" {
		t.Fatalf("proxy update failed: %+v %v", updated, err)
	}
	settings, err := s.Settings(testContext)
	if err != nil || len(settings.Proxies) != 2 || settings.ProviderProxies["tvb"] != core.DirectProxy || settings.ProviderProxies["hoy"] != core.DirectProxy {
		t.Fatalf("settings = %+v, %v", settings, err)
	}
	ch, err := s.SaveChannel(testContext, core.Channel{Name: "频道", SourceType: "stream", URL: "https://example.com/live.m3u8", Enabled: true, Proxy: first.ID})
	if err != nil || core.EffectiveProxy(ch, settings) != "socks5://10.10.1.38:1081" {
		t.Fatalf("channel proxy = %+v, %v", ch, err)
	}
	sub, err := s.SaveSubscription(testContext, core.Subscription{Name: "订阅", URL: "https://example.com/list.m3u", IntervalMinutes: 60, Enabled: true, Proxy: second.ID})
	if err != nil || sub.Proxy != second.ID {
		t.Fatalf("subscription proxy = %+v, %v", sub, err)
	}
	if _, err := s.SaveSubscription(testContext, core.Subscription{Name: "坏订阅", URL: "https://example.com/other.m3u", IntervalMinutes: 60, Enabled: true, Proxy: "ffffffffffffffffffffffff"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("subscription with unknown proxy accepted: %v", err)
	}
	if err := s.SetProviderProxy(testContext, "tvb", first.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProviderProxy(testContext, "unknown", first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown provider accepted: %v", err)
	}
	for _, ref := range []string{"ffffffffffffffffffffffff", "socks5://x.example:1", "inherit"} {
		if err := s.SetProviderProxy(testContext, "tvb", ref); !errors.Is(err, ErrValidation) {
			t.Fatalf("provider proxy %q accepted: %v", ref, err)
		}
	}
	result, err := s.AddChannels(testContext, core.BulkChannelRequest{SourceType: "builtin", ProviderID: "tvb", ChannelIDs: []string{"C"}, Proxy: second.ID})
	if err != nil || result.Added != 1 {
		t.Fatalf("builtin add = %+v, %v", result, err)
	}
	settings, _ = s.Settings(testContext)
	builtin, err := s.Channel(testContext, result.Results[0].ChannelID)
	if err != nil || builtin.Proxy != core.DirectProxy || core.EffectiveProxy(builtin, settings) != "socks5://10.10.1.38:1081" {
		t.Fatalf("builtin channel did not follow the provider proxy: %+v %v", builtin, err)
	}
	var inUse *ProxyInUseError
	if err := s.DeleteProxy(testContext, first.ID); !errors.As(err, &inUse) || inUse.Channels != 1 || inUse.Subscriptions != 0 || inUse.Providers != 1 {
		t.Fatalf("in-use delete = %v", err)
	}
	if err := s.DeleteProxy(testContext, second.ID); !errors.As(err, &inUse) || inUse.Channels != 0 || inUse.Subscriptions != 1 || inUse.Providers != 0 {
		t.Fatalf("in-use delete = %v", err)
	}
	settings, _ = s.Settings(testContext)
	if len(settings.Proxies) != 2 {
		t.Fatal("refused delete modified proxies")
	}
	ch.Proxy = core.DirectProxy
	if _, err := s.SaveChannel(testContext, ch); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProviderProxy(testContext, "tvb", ""); err != nil {
		t.Fatal(err)
	}
	sub.Proxy = ""
	if _, err := s.SaveSubscription(testContext, sub); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first.ID, second.ID} {
		if err := s.DeleteProxy(testContext, id); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteProxy(testContext, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second delete = %v", err)
		}
	}
	settings, _ = s.Settings(testContext)
	if len(settings.Proxies) != 0 || settings.ProviderProxies["tvb"] != core.DirectProxy {
		t.Fatalf("settings after delete = %+v", settings)
	}
	if core.EffectiveProxy(core.Channel{Proxy: "ffffffffffffffffffffffff"}, settings) == "" {
		t.Fatal("dangling proxy reference fell back to a direct connection")
	}
}

func TestBulkAddUsesRequestedProxy(t *testing.T) {
	s := testStore(t)
	p, err := s.SaveProxy(testContext, core.Proxy{Name: "代理", Scheme: "http", Host: "proxy.example", Port: 3128})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.AddChannels(testContext, core.BulkChannelRequest{SourceType: "stream", Text: "https://example.com/a.m3u8", Proxy: p.ID})
	if err != nil || result.Added != 1 {
		t.Fatalf("bulk add = %+v, %v", result, err)
	}
	if ch, err := s.Channel(testContext, result.Results[0].ChannelID); err != nil || ch.Proxy != p.ID {
		t.Fatalf("bulk channel proxy = %+v, %v", ch, err)
	}
	for _, ref := range []string{"ffffffffffffffffffffffff", "http://x.example:1", "inherit"} {
		if _, err := s.AddChannels(testContext, core.BulkChannelRequest{SourceType: "stream", Text: "https://example.com/b.m3u8", Proxy: ref}); !errors.Is(err, ErrValidation) {
			t.Fatalf("bulk proxy %q accepted: %v", ref, err)
		}
	}
	if _, err := s.Channel(testContext, result.Results[0].ChannelID); err != nil {
		t.Fatal("failed bulk request removed existing channel")
	}
}

func TestSchemaV3ProxyMigrationAndV3Backup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	youtube, err := s.SaveChannel(testContext, core.Channel{Name: "YT", URL: "https://www.youtube.com/watch?v=vr3XyVCR4T0", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	streamA, err := s.SaveChannel(testContext, core.Channel{Name: "A", SourceType: "stream", URL: "https://a.example/live.m3u8", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	streamD, err := s.SaveChannel(testContext, core.Channel{Name: "D", SourceType: "stream", URL: "https://d.example/live.m3u8", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	tvb, err := s.AddChannels(testContext, core.BulkChannelRequest{SourceType: "builtin", ProviderID: "tvb", ChannelIDs: []string{"C", "F"}})
	if err != nil {
		t.Fatal(err)
	}
	hk, err := s.AddChannels(testContext, core.BulkChannelRequest{SourceType: "builtin", ProviderID: "hkstv", ChannelIDs: []string{"mutfysrq"}})
	if err != nil {
		t.Fatal(err)
	}
	sub1, err := s.SaveSubscription(testContext, core.Subscription{Name: "一", URL: "https://example.com/1.m3u", IntervalMinutes: 60, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	sub2, err := s.SaveSubscription(testContext, core.Subscription{Name: "二", URL: "https://example.com/2.m3u", IntervalMinutes: 60, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	settings, err := s.Settings(testContext)
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite the database into its version 3 shape: URL and inherit proxies, a global proxy URL.
	legacy := map[string]any{"base_url": "", "default_mode": "relay", "default_quality": 720, "upstream_proxy": "http://global.example:3128", "monthly_budget_gb": 800, "playback_token": settings.PlaybackToken}
	payload, _ := json.Marshal(legacy)
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE settings SET payload=? WHERE id=1`, []any{string(payload)}},
		{`UPDATE channels SET proxy='inherit' WHERE id IN (?,?,?)`, []any{youtube.ID, hk.Results[0].ChannelID, tvb.Results[1].ChannelID}},
		{`UPDATE channels SET proxy='socks5://a.example:1080' WHERE id=?`, []any{streamA.ID}},
		{`UPDATE channels SET proxy='http://user:pw@b.example:8080' WHERE id=?`, []any{tvb.Results[0].ChannelID}},
		{`UPDATE channels SET proxy='direct' WHERE id=?`, []any{streamD.ID}},
		{`PRAGMA user_version=3`, nil},
	} {
		if _, err := s.db.Exec(stmt.sql, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	sub1.Proxy, sub2.Proxy = "inherit", "socks5://a.example:1080"
	for _, sub := range []core.Subscription{sub1, sub2} {
		if err := writeSubscription(testContext, s.db, sub); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	settings, err = upgraded.Settings(testContext)
	if err != nil || len(settings.Proxies) != 3 || settings.LegacyProxy != "" {
		t.Fatalf("migrated settings = %+v, %v", settings, err)
	}
	byURL := map[string]core.Proxy{}
	for _, p := range settings.Proxies {
		byURL[p.URL()] = p
	}
	global, a, b := byURL["http://global.example:3128"], byURL["socks5://a.example:1080"], byURL["http://user:pw@b.example:8080"]
	if global.ID == "" || a.ID == "" || b.ID == "" || global.Name != "global.example:3128" || a.Name != "a.example:1080" || b.Username != "user" || b.Password != "pw" {
		t.Fatalf("converted proxies = %+v", settings.Proxies)
	}
	channels, err := upgraded.Channels(testContext)
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]string{}
	for _, ch := range channels {
		refs[ch.ID] = ch.Proxy
	}
	if refs[youtube.ID] != global.ID || refs[streamA.ID] != a.ID || refs[streamD.ID] != core.DirectProxy || refs[tvb.Results[0].ChannelID] != core.DirectProxy || refs[tvb.Results[1].ChannelID] != core.DirectProxy || refs[hk.Results[0].ChannelID] != core.DirectProxy {
		t.Fatalf("channel references = %v", refs)
	}
	if settings.ProviderProxies["tvb"] != b.ID || settings.ProviderProxies["hkstv"] != global.ID || settings.ProviderProxies["tdm"] != core.DirectProxy {
		t.Fatalf("provider proxies = %v", settings.ProviderProxies)
	}
	subs, err := upgraded.Subscriptions(testContext)
	if err != nil || len(subs) != 2 {
		t.Fatal(err)
	}
	for _, sub := range subs {
		if (sub.ID == sub1.ID && sub.Proxy != global.ID) || (sub.ID == sub2.ID && sub.Proxy != a.ID) {
			t.Fatalf("subscription proxy not converted: %+v", sub)
		}
	}
	var version int
	if err := upgraded.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 4 {
		t.Fatal("schema not upgraded to version 4")
	}
	before, err := upgraded.Export(testContext)
	if err != nil || before.Version != 4 {
		t.Fatal(err)
	}
	if err := upgraded.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if again, err := reopened.Export(testContext); err != nil || !reflect.DeepEqual(before, again) {
		t.Fatal("migration not idempotent")
	}

	// A version 3 backup receives the same conversion; version 2 is no longer accepted.
	token := strings.Repeat("a", 40)
	v3 := core.Backup{Version: 3, Settings: core.Settings{DefaultMode: "relay", DefaultQuality: 720, MonthlyBudgetGB: 800, PlaybackToken: token, LegacyProxy: "socks5://g.example:1080"},
		Channels: []core.Channel{
			{ID: "aaaaaaaaaaaa", Name: "x", SourceType: "stream", URL: "https://x.example/live.m3u8", Enabled: true, Proxy: "inherit", SortOrder: 0},
			{ID: "bbbbbbbbbbbb", Name: "C", SourceType: "builtin", ProviderID: "tvb", ProviderChannelID: "C", URL: "https://news.tvb.com/tc/live/C", Enabled: true, Proxy: "http://b.example:8080", SortOrder: 1},
		},
		Subscriptions: []core.Subscription{{ID: "cccccccccccc", Name: "订阅", URL: "https://example.com/list.m3u", IntervalMinutes: 60, Enabled: true, Proxy: "inherit"}},
	}
	fresh := testStore(t)
	if err := fresh.Import(testContext, v3); err != nil {
		t.Fatal(err)
	}
	imported, err := fresh.Export(testContext)
	if err != nil || len(imported.Settings.Proxies) != 2 {
		t.Fatalf("imported = %+v, %v", imported.Settings, err)
	}
	byURL = map[string]core.Proxy{}
	for _, p := range imported.Settings.Proxies {
		byURL[p.URL()] = p
	}
	g, bb := byURL["socks5://g.example:1080"], byURL["http://b.example:8080"]
	if imported.Channels[0].Proxy != g.ID || imported.Channels[1].Proxy != core.DirectProxy || imported.Settings.ProviderProxies["tvb"] != bb.ID || imported.Subscriptions[0].Proxy != g.ID {
		t.Fatalf("v3 backup conversion = %+v", imported)
	}
	v2 := v3
	v2.Version = 2
	if err := fresh.Import(testContext, v2); !errors.Is(err, ErrValidation) {
		t.Fatalf("version 2 backup accepted: %v", err)
	}
	dangling := imported
	dangling.Channels = append([]core.Channel(nil), imported.Channels...)
	dangling.Channels[0].Proxy = "ffffffffffffffffffffffff"
	if err := fresh.Import(testContext, dangling); !errors.Is(err, ErrValidation) {
		t.Fatalf("dangling proxy backup accepted: %v", err)
	}
	if after, err := fresh.Export(testContext); err != nil || !reflect.DeepEqual(imported, after) {
		t.Fatal("rejected backup modified data")
	}
}
