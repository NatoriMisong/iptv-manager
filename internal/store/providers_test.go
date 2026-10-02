package store

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"iptv-manager/internal/core"
	"iptv-manager/internal/source"
)

func downgradeProviderFixture(t *testing.T, s *Store) {
	t.Helper()
	for _, stmt := range []string{
		`UPDATE channels SET source_type=CASE WHEN provider_id='tvb' THEN 'tvb' ELSE 'stream' END WHERE source_type='builtin'`,
		`ALTER TABLE channels DROP COLUMN provider_id`,
		`ALTER TABLE channels DROP COLUMN provider_channel_id`,
		`PRAGMA user_version=2`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProviderMigrationPreservesExistingConfigurationAndV2Backup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range [][2]string{{"tvb", "C"}, {"tvb", "F"}, {"hkstv", "mutfysrq"}, {"tdm", "ctvp"}} {
		_, err := s.SaveChannel(testContext, core.Channel{Name: "自定义名称 " + identity[1], Group: "我的分组", SourceType: "builtin", ProviderID: identity[0], ProviderChannelID: identity[1], Mode: "direct", Proxy: "socks5://proxy.example:1080", Enabled: false})
		if err != nil {
			t.Fatal(err)
		}
	}
	hkstv := "https://webcast.hkstv.tv/livestream/mutfysrq/playlist.m3u8"
	_, err = s.SaveChannel(testContext, core.Channel{Name: "自定义源", SourceType: "stream", URL: hkstv + "?custom=yes", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := s.SaveSubscription(testContext, core.Subscription{Name: "订阅", URL: "https://example.com/list.m3u", IntervalMinutes: 60, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ApplySubscription(testContext, sub, source.Playlist{Entries: []source.Entry{{Key: "id:hkstv", Name: "订阅频道", URL: hkstv}}})
	if err != nil {
		t.Fatal(err)
	}
	preservedHash := strings.Repeat("a", 60)
	if err = s.SetAdminHash(testContext, preservedHash); err != nil {
		t.Fatal(err)
	}
	if err = s.AddTraffic(testContext, "2026-10", 12345); err != nil {
		t.Fatal(err)
	}
	before, err := s.Export(testContext)
	if err != nil {
		t.Fatal(err)
	}
	downgradeProviderFixture(t, s)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := upgraded.Export(testContext)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("migration changed channel IDs, URLs, user settings or subscription ownership")
	}
	hash, err := upgraded.AdminHash(testContext)
	if err != nil || hash != preservedHash {
		t.Fatal("admin credentials changed")
	}
	traffic, err := upgraded.Traffic(testContext, "2026-10")
	if err != nil || traffic.Bytes != 12345 {
		t.Fatal("traffic changed")
	}
	var version int
	if err = upgraded.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 3 {
		t.Fatal("schema not upgraded")
	}
	if err = upgraded.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	again, err := reopened.Export(testContext)
	if err != nil || !reflect.DeepEqual(after, again) {
		t.Fatal("migration not idempotent")
	}
	// A version 2 configuration backup gets the same identity conversion.
	legacy := before
	legacy.Version = 2
	legacy.Channels = append([]core.Channel(nil), before.Channels...)
	for i, ch := range legacy.Channels {
		if ch.IsBuiltin() {
			if ch.ProviderID == "tvb" {
				ch.SourceType = "tvb"
			} else {
				ch.SourceType = "stream"
			}
			ch.ProviderID = ""
			ch.ProviderChannelID = ""
		}
		legacy.Channels[i] = ch
	}
	restored := testStore(t)
	if err = restored.Import(testContext, legacy); err != nil {
		t.Fatal(err)
	}
	imported, err := restored.Export(testContext)
	if err != nil || !reflect.DeepEqual(imported.Channels, before.Channels) || !reflect.DeepEqual(imported.Settings, before.Settings) {
		t.Fatal("v2 backup conversion changed identity or settings")
	}
}

func TestProviderMigrationFailureRollsBackSchemaAndData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	downgradeProviderFixture(t, s)
	if _, err = s.db.Exec(`UPDATE channels SET source_type='tvb',url='https://news.tvb.com/tc/live/unknown' WHERE sort_order=0`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if opened, err := Open(path); err == nil {
		opened.Close()
		t.Fatal("invalid legacy source accepted")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, columns int
	var raw string
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 2 {
		t.Fatal("failed migration changed schema version")
	}
	if err = db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('channels') WHERE name LIKE 'provider_%'`).Scan(&columns); err != nil || columns != 0 {
		t.Fatal("failed migration left columns behind")
	}
	if err = db.QueryRow(`SELECT url FROM channels WHERE sort_order=0`).Scan(&raw); err != nil || raw != "https://news.tvb.com/tc/live/unknown" {
		t.Fatal("failed migration modified data")
	}
}

func TestBuiltinIdentityCannotBeChangedByEditingAndBackupIsAtomic(t *testing.T) {
	s := testStore(t)
	result, err := s.AddChannels(testContext, core.BulkChannelRequest{SourceType: "builtin", ProviderID: "tvb", ChannelIDs: []string{"C"}})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := s.Channel(testContext, result.Results[0].ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	changed := ch
	changed.SourceType = "stream"
	changed.ProviderID = "hkstv"
	changed.ProviderChannelID = "mutfysrq"
	changed.URL = "https://other.example/live"
	changed.Name = "改名"
	changed.Mode = "direct"
	changed.Proxy = "direct"
	changed.Enabled = false
	saved, err := s.SaveChannel(testContext, changed)
	if err != nil || saved.ID != ch.ID || saved.URL != ch.URL || saved.SourceType != ch.SourceType || saved.ProviderID != ch.ProviderID || saved.ProviderChannelID != ch.ProviderChannelID || saved.Name != "改名" || saved.Mode != "direct" || saved.Enabled {
		t.Fatalf("edit lost identity or settings: %+v %v", saved, err)
	}
	before, err := s.Export(testContext)
	if err != nil {
		t.Fatal(err)
	}
	bad := before
	bad.Channels = append([]core.Channel(nil), before.Channels...)
	bad.Channels[len(bad.Channels)-1].ProviderChannelID = "unknown"
	if err = s.Import(testContext, bad); err == nil {
		t.Fatal("unknown builtin backup accepted")
	}
	after, err := s.Export(testContext)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("invalid backup partially applied")
	}
}
