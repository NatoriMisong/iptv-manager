package store

import (
	"errors"
	"reflect"
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

func TestImportRejectsOtherVersionsAndDanglingProxies(t *testing.T) {
	s := testStore(t)
	p, err := s.SaveProxy(testContext, core.Proxy{Name: "代理", Scheme: "socks5", Host: "proxy.example", Port: 1080})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveChannel(testContext, core.Channel{Name: "频道", SourceType: "stream", URL: "https://example.com/live.m3u8", Enabled: true, Proxy: p.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProviderProxy(testContext, "tvb", p.ID); err != nil {
		t.Fatal(err)
	}
	before, err := s.Export(testContext)
	if err != nil || before.Version != 5 {
		t.Fatalf("export = %+v, %v", before, err)
	}
	for _, version := range []int{2, 3, 4, 6} {
		old := before
		old.Version = version
		if err := s.Import(testContext, old); !errors.Is(err, ErrValidation) {
			t.Fatalf("backup version %d accepted: %v", version, err)
		}
	}
	dangling := before
	dangling.Channels = append([]core.Channel(nil), before.Channels...)
	dangling.Channels[len(dangling.Channels)-1].Proxy = "ffffffffffffffffffffffff"
	if err := s.Import(testContext, dangling); !errors.Is(err, ErrValidation) {
		t.Fatalf("dangling channel proxy accepted: %v", err)
	}
	orphan := before
	orphan.Settings.ProviderProxies = map[string]string{"tvb": "ffffffffffffffffffffffff"}
	if err := s.Import(testContext, orphan); !errors.Is(err, ErrValidation) {
		t.Fatalf("dangling provider proxy accepted: %v", err)
	}
	after, err := s.Export(testContext)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("rejected backup modified data")
	}
	restored := testStore(t)
	if err := restored.Import(testContext, before); err != nil {
		t.Fatal(err)
	}
	if again, err := restored.Export(testContext); err != nil || !reflect.DeepEqual(before.Settings.Proxies, again.Settings.Proxies) || again.Settings.ProviderProxies["tvb"] != p.ID {
		t.Fatalf("proxies not restored: %+v %v", again.Settings, err)
	}
}
