package store

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"iptv-manager/internal/core"
	"iptv-manager/internal/source"
)

func TestLegacySchemaAndBackupUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.Export(testContext)
	if err := s.SetAdminHash(testContext, strings.Repeat("x", 60)); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTraffic(testContext, "2026-10", 123); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{`DROP INDEX channels_subscription_key`, `ALTER TABLE channels DROP COLUMN source_type`, `ALTER TABLE channels DROP COLUMN subscription_id`, `ALTER TABLE channels DROP COLUMN source_key`, `ALTER TABLE channels DROP COLUMN source_missing`, `DROP TABLE subscriptions`, `PRAGMA user_version=1`} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if hash, err := s.AdminHash(testContext); err != nil || hash != strings.Repeat("x", 60) {
		t.Fatal("migration changed admin hash")
	}
	if traffic, err := s.Traffic(testContext, "2026-10"); err != nil || traffic.Bytes != 123 {
		t.Fatal("migration changed traffic")
	}
	after, err := s.Export(testContext)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("migration modified channels: %+v %v", after, err)
	}
	legacy := before
	legacy.Version = 1
	for i := range legacy.Channels {
		legacy.Channels[i].SourceType = ""
	}
	if err := s.Import(testContext, legacy); err != nil {
		t.Fatal(err)
	}
	loaded, _ := s.Channels(testContext)
	if len(loaded) != 2 || loaded[0].SourceType != "youtube" {
		t.Fatalf("legacy backup: %+v", loaded)
	}
}
func TestStreamChannelsAndBulk(t *testing.T) {
	s := testStore(t)
	ch, err := s.SaveChannel(testContext, core.Channel{Name: "通用", SourceType: "stream", URL: "https://cdn.example/live.m3u8?token=a%2Bb", Enabled: true, Quality: 720})
	if err != nil || ch.Quality != 0 || !ch.IsStream() {
		t.Fatalf("stream: %+v %v", ch, err)
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
func TestSubscriptionReconciliationAndBackup(t *testing.T) {
	s := testStore(t)
	sub, err := s.SaveSubscription(testContext, core.Subscription{Name: "新闻", URL: "https://example.com/list.m3u", IntervalMinutes: 60, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	list := source.Playlist{Entries: []source.Entry{{Key: "id:one", Name: "One", URL: "https://cdn.example/old.m3u8"}, {Key: "id:two", Name: "Two", URL: "http://cdn.example/two.ts"}}}
	ids, err := s.ApplySubscription(testContext, sub, list)
	if err != nil || len(ids) != 2 {
		t.Fatalf("sync: %v %v", ids, err)
	}
	first, _ := s.Channel(testContext, ids[0])
	second, _ := s.Channel(testContext, ids[1])
	first.Mode = "direct"
	first.Proxy = "direct"
	first.Enabled = false
	first, err = s.SaveChannel(testContext, first)
	if err != nil {
		t.Fatal(err)
	}
	list.Entries = list.Entries[:1]
	list.Entries[0].URL = "https://cdn.example/new.m3u8?token=changed"
	list.Entries[0].Name = "Updated"
	if _, err := s.ApplySubscription(testContext, sub, list); err != nil {
		t.Fatal(err)
	}
	updated, _ := s.Channel(testContext, first.ID)
	missing, _ := s.Channel(testContext, second.ID)
	if updated.ID != first.ID || updated.Mode != "direct" || updated.Enabled || updated.Proxy != "direct" || updated.URL != list.Entries[0].URL || !missing.SourceMissing {
		t.Fatalf("local settings or identity lost: %+v %+v", updated, missing)
	}
	list.Entries = append(list.Entries, source.Entry{Key: "id:two", Name: "Two", URL: "http://cdn.example/two.ts"})
	if _, err := s.ApplySubscription(testContext, sub, list); err != nil {
		t.Fatal(err)
	}
	reappeared, _ := s.Channel(testContext, second.ID)
	if reappeared.SourceMissing || !reappeared.Enabled {
		t.Fatal("missing channel did not recover")
	}
	backup, _ := s.Export(testContext)
	bad := source.Playlist{Entries: []source.Entry{{Key: "id:one", Name: "Should rollback", URL: "https://cdn.example/temporary"}, {Key: "id:one", Name: "Duplicate", URL: "https://cdn.example/duplicate"}}}
	if _, err := s.ApplySubscription(testContext, sub, bad); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid update accepted: %v", err)
	}
	unchanged, _ := s.Export(testContext)
	if !reflect.DeepEqual(backup, unchanged) {
		t.Fatal("partially applied invalid subscription update")
	}
	if backup.Version != 2 || len(backup.Subscriptions) != 1 {
		t.Fatalf("backup: %+v", backup)
	}
	other := testStore(t)
	if err := other.Import(testContext, backup); err != nil {
		t.Fatal(err)
	}
	copy, _ := other.Export(testContext)
	if !reflect.DeepEqual(copy.Channels, backup.Channels) || copy.Subscriptions[0].URL != sub.URL {
		t.Fatal("subscription backup lost data")
	}
	current, _ := s.Subscription(testContext, sub.ID)
	current.Name = "Changed"
	current, err = s.SaveSubscription(testContext, current)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplySubscription(testContext, sub, list); !errors.Is(err, ErrSubscriptionChanged) {
		t.Fatalf("stale update applied: %v", err)
	}
	if _, err := s.ApplySubscription(testContext, current, source.Playlist{}); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty list erased state: %v", err)
	}
	if err := s.DeleteSubscription(testContext, sub.ID); err != nil {
		t.Fatal(err)
	}
	remaining, _ := s.Channels(testContext)
	if len(remaining) != 2 {
		t.Fatalf("subscription delete touched manual channels: %+v", remaining)
	}
}
