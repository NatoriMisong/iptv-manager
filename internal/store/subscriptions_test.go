package store

import (
	"errors"
	"reflect"
	"testing"

	"iptv-manager/internal/core"
	"iptv-manager/internal/source"
)

func TestStreamChannelsAndBulk(t *testing.T) {
	s := testStore(t)
	ch, err := s.SaveChannel(testContext, core.Channel{Name: "通用", SourceType: "stream", URL: "https://cdn.example/live.m3u8?token=a%2Bb", Enabled: true, Quality: 720})
	if err != nil || ch.Quality != 0 || !ch.IsStream() {
		t.Fatalf("stream: %+v %v", ch, err)
	}
	verbatim, err := s.SaveChannel(testContext, core.Channel{Name: "片段", SourceType: "stream", URL: "https://CDN.example/live/{台}|a.php?shk_cid=hdgd01#.m3u8", Enabled: true})
	if err != nil || verbatim.URL != "https://cdn.example/live/{台}|a.php?shk_cid=hdgd01#.m3u8" {
		t.Fatalf("stream URL re-encoded: %+v %v", verbatim, err)
	}
	result, err := s.AddChannels(testContext, core.BulkChannelRequest{SourceType: "stream", Text: "https://cdn.example/live.m3u8?token=a%2Bb\n另一个,http://media.example:8080/live.ts\nhttps://cdn.example/other.m3u8"})
	if err != nil || result.Added != 2 || result.Skipped != 1 {
		t.Fatalf("bulk stream: %+v %v", result, err)
	}
	for _, kind := range []string{"youtube", "bad-type"} {
		ch.ID = ""
		ch.SourceType = kind
		if _, err := s.SaveChannel(testContext, ch); !errors.Is(err, ErrValidation) {
			t.Fatalf("source validation: %v", err)
		}
	}
}
func TestSubscriptionCatalogueAndBackup(t *testing.T) {
	s := testStore(t)
	proxy, err := s.SaveProxy(testContext, core.Proxy{Name: "出口", Scheme: "socks5", Host: "proxy.example", Port: 1080})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []core.Subscription{
		{Name: "无地址", IntervalMinutes: 60},
		{Name: "重复", Sources: []core.SubscriptionSource{{URL: "https://a.example/l.m3u"}, {URL: "https://a.example/l.m3u"}}, IntervalMinutes: 60},
		{Name: "内网", Sources: []core.SubscriptionSource{{URL: "http://10.0.0.1/l.m3u"}}, IntervalMinutes: 60},
		{Name: "规则", Sources: []core.SubscriptionSource{{URL: "https://a.example/l.m3u"}}, IntervalMinutes: 60, Defaults: []core.DomainDefault{{Domain: "cdn.sub.example.com", Mode: "relay"}}},
		{Name: "规则代理", Sources: []core.SubscriptionSource{{URL: "https://a.example/l.m3u"}}, IntervalMinutes: 60, Defaults: []core.DomainDefault{{Domain: "example.com", Mode: "relay", Proxy: "ffffffffffffffffffffffff"}}},
	} {
		if _, err := s.SaveSubscription(testContext, bad); !errors.Is(err, ErrValidation) {
			t.Fatalf("accepted %+v: %v", bad, err)
		}
	}
	sub, err := s.SaveSubscription(testContext, core.Subscription{Name: "新闻", Sources: []core.SubscriptionSource{{URL: "https://example.com/list.m3u"}, {URL: "https://backup.example/list.m3u"}}, IntervalMinutes: 60, Enabled: true})
	if err != nil || len(sub.Sources) != 2 || sub.Sources[0].ID == "" || sub.Sources[0].ID == sub.Sources[1].ID {
		t.Fatalf("save: %+v %v", sub, err)
	}
	primary, secondary := sub.Sources[0].ID, sub.Sources[1].ID
	list := source.Playlist{Skipped: 3, Entries: []source.Entry{{Name: "One", URL: "https://a.cdn.example/old.m3u8", Group: "A", Logo: "https://cdn.example/one.png"}, {Name: "Two", URL: "http://cdn2.example/two.ts", Group: "B"}}}
	if _, err := s.ApplySource(testContext, sub, primary, list); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplySource(testContext, sub, secondary, source.Playlist{Entries: []source.Entry{{Name: "One", URL: "https://mirror.example/one.m3u8", Group: "镜像"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplySource(testContext, sub, "unknown0", list); !errors.Is(err, ErrSubscriptionChanged) {
		t.Fatalf("unknown source applied: %v", err)
	}
	synced, _ := s.Subscription(testContext, sub.ID)
	if synced.Sources[0].Entries != 2 || synced.Sources[0].Skipped != 3 || synced.Sources[0].LastSync.IsZero() || synced.Sources[1].Entries != 1 {
		t.Fatalf("source status: %+v", synced.Sources)
	}
	channels, _ := s.Channels(testContext)
	if len(channels) != 2 {
		t.Fatalf("sync created channels: %+v", channels)
	}
	entries, err := s.SubscriptionEntries(testContext, sub.ID)
	if err != nil || len(entries) != 3 || entries[0].Added {
		t.Fatalf("entries: %+v %v", entries, err)
	}

	for _, bad := range []core.SubscriptionAddRequest{
		{},
		{Items: []core.SubscriptionAddItem{{SourceID: "nope", Name: "One"}}},
		{Items: []core.SubscriptionAddItem{{SourceID: primary, Name: "One"}, {SourceID: primary, Name: "One"}}},
		{Items: []core.SubscriptionAddItem{{SourceID: primary, Name: "One"}}, Group: "a\"b"},
		{Items: []core.SubscriptionAddItem{{SourceID: primary, Name: "One"}}, Defaults: []core.DomainDefault{{Domain: "cdn.example", Mode: "auto"}}},
		{Items: []core.SubscriptionAddItem{{SourceID: primary, Name: "One"}}, Defaults: []core.DomainDefault{{Domain: "cdn.example", Proxy: "ffffffffffffffffffffffff"}}},
	} {
		if _, err := s.AddSubscriptionChannels(testContext, sub.ID, bad); !errors.Is(err, ErrValidation) {
			t.Fatalf("accepted %+v: %v", bad, err)
		}
	}
	if _, err := s.AddSubscriptionChannels(testContext, "missing00000", core.SubscriptionAddRequest{Items: []core.SubscriptionAddItem{{SourceID: primary, Name: "One"}}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown subscription: %v", err)
	}
	req := core.SubscriptionAddRequest{
		Items:    []core.SubscriptionAddItem{{SourceID: primary, Name: "One"}, {SourceID: primary, Name: "Two"}, {SourceID: secondary, Name: "One"}, {SourceID: primary, Name: "Gone"}},
		Defaults: []core.DomainDefault{{Domain: "cdn.example", Mode: "direct", Proxy: proxy.ID}, {Domain: "cdn2.example", Mode: "relay", Proxy: "direct"}},
	}
	result, err := s.AddSubscriptionChannels(testContext, sub.ID, req)
	if err != nil || result.Added != 3 || result.Failed != 1 || result.Skipped != 0 || result.Results[3].Status != "failed" {
		t.Fatalf("add: %+v %v", result, err)
	}
	again, err := s.AddSubscriptionChannels(testContext, sub.ID, core.SubscriptionAddRequest{Items: req.Items[:1], Group: "统一"})
	if err != nil || again.Skipped != 1 || again.Added != 0 {
		t.Fatalf("re-add: %+v %v", again, err)
	}
	one, _ := s.Channel(testContext, result.Results[0].ChannelID)
	two, _ := s.Channel(testContext, result.Results[1].ChannelID)
	mirror, _ := s.Channel(testContext, result.Results[2].ChannelID)
	if one.Name != "One" || one.URL != "https://a.cdn.example/old.m3u8" || one.Group != "A" || one.SourceGroup != "A" || one.Logo != "https://cdn.example/one.png" || one.Mode != "direct" || one.Proxy != proxy.ID || !one.Enabled || one.SourceKey != "One" || one.SourceID != primary {
		t.Fatalf("one: %+v", one)
	}
	if two.Mode != "relay" || two.Proxy != "direct" || two.Group != "B" || two.SortOrder != one.SortOrder+1 {
		t.Fatalf("two: %+v", two)
	}
	if mirror.SourceID != secondary || mirror.URL != "https://mirror.example/one.m3u8" || mirror.Mode != "relay" {
		t.Fatalf("mirror: %+v", mirror)
	}
	remembered, _ := s.Subscription(testContext, sub.ID)
	if len(remembered.Defaults) != 2 {
		t.Fatalf("defaults not remembered: %+v", remembered.Defaults)
	}
	entries, _ = s.SubscriptionEntries(testContext, sub.ID)
	added := 0
	for _, e := range entries {
		if e.Added {
			added++
		}
	}
	if added != 3 {
		t.Fatalf("added flags: %+v", entries)
	}

	one.Group, one.Enabled = "我的分组", false
	if one, err = s.SaveChannel(testContext, one); err != nil || one.Group != "我的分组" {
		t.Fatalf("group edit: %+v %v", one, err)
	}
	list.Entries = []source.Entry{{Name: "One", URL: "https://cdn.example/new.m3u8?token=changed", Group: "A2", Logo: ""}, {Name: "Three", URL: "https://cdn.example/three.m3u8"}}
	changed, err := s.ApplySource(testContext, remembered, primary, list)
	if err != nil || len(changed) != 2 {
		t.Fatalf("resync: %v %v", changed, err)
	}
	updated, _ := s.Channel(testContext, one.ID)
	missing, _ := s.Channel(testContext, two.ID)
	if updated.URL != "https://cdn.example/new.m3u8?token=changed" || updated.Logo != "" || updated.SourceGroup != "A2" || updated.Group != "我的分组" || updated.Enabled || updated.Mode != "direct" || updated.Proxy != proxy.ID {
		t.Fatalf("resync lost user settings or did not refresh media: %+v", updated)
	}
	if !missing.SourceMissing || missing.URL != "http://cdn2.example/two.ts" {
		t.Fatalf("missing: %+v", missing)
	}
	list.Entries = append(list.Entries, source.Entry{Name: "Two", URL: "http://cdn2.example/two-new.ts"})
	if _, err := s.ApplySource(testContext, remembered, primary, list); err != nil {
		t.Fatal(err)
	}
	reappeared, _ := s.Channel(testContext, two.ID)
	if reappeared.SourceMissing || reappeared.URL != "http://cdn2.example/two-new.ts" || !reappeared.Enabled {
		t.Fatalf("missing channel did not recover: %+v", reappeared)
	}

	backup, _ := s.Export(testContext)
	if _, err := s.ApplySource(testContext, remembered, primary, source.Playlist{Entries: []source.Entry{{Name: "Dup", URL: "https://cdn.example/a"}, {Name: "Dup", URL: "https://cdn.example/b"}}}); !errors.Is(err, ErrValidation) {
		t.Fatalf("duplicate names accepted: %v", err)
	}
	if _, err := s.ApplySource(testContext, remembered, primary, source.Playlist{}); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty list erased state: %v", err)
	}
	unchanged, _ := s.Export(testContext)
	if !reflect.DeepEqual(backup, unchanged) || backup.Version != 6 || len(backup.Subscriptions) != 1 {
		t.Fatalf("backup: %+v", backup)
	}
	other := testStore(t)
	if err := other.Import(testContext, backup); err != nil {
		t.Fatal(err)
	}
	copy, _ := other.Export(testContext)
	if !reflect.DeepEqual(copy.Channels, backup.Channels) || len(copy.Subscriptions[0].Sources) != 2 || copy.Subscriptions[0].Sources[0].ID != primary {
		t.Fatalf("subscription backup lost data: %+v", copy.Subscriptions)
	}
	broken := backup
	broken.Channels = append([]core.Channel(nil), backup.Channels...)
	broken.Channels[len(broken.Channels)-1].SourceID = "unknown0"
	if err := other.Import(testContext, broken); !errors.Is(err, ErrValidation) {
		t.Fatalf("dangling source accepted: %v", err)
	}

	current, _ := s.Subscription(testContext, sub.ID)
	current.Sources = current.Sources[:1]
	current, err = s.SaveSubscription(testContext, current)
	if err != nil || len(current.Sources) != 1 || current.Sources[0].ID != primary || current.Sources[0].Entries != 3 {
		t.Fatalf("edit kept status: %+v %v", current, err)
	}
	if _, err := s.Channel(testContext, mirror.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed list kept its channel: %v", err)
	}
	if _, err := s.ApplySource(testContext, remembered, primary, list); !errors.Is(err, ErrSubscriptionChanged) {
		t.Fatalf("stale update applied: %v", err)
	}
	cleared, err := s.ClearSubscription(testContext, sub.ID)
	if err != nil || len(cleared) != 2 {
		t.Fatalf("clear: %v %v", cleared, err)
	}
	if entries, _ = s.SubscriptionEntries(testContext, sub.ID); len(entries) != 3 {
		t.Fatalf("clear removed catalogue: %+v", entries)
	}
	if err := s.DeleteSubscription(testContext, sub.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubscriptionEntries(testContext, sub.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("catalogue outlived subscription: %v", err)
	}
	remaining, _ := s.Channels(testContext)
	if len(remaining) != 2 {
		t.Fatalf("subscription delete touched manual channels: %+v", remaining)
	}
}
