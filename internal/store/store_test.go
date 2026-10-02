package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"iptv-manager/internal/core"
)

var testContext = context.Background()

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state", "iptv-manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestSeedAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	channels, err := s.Channels(testContext)
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 2 || channels[0].ID == channels[1].ID || !channels[0].Enabled || !channels[1].Enabled {
		t.Fatalf("invalid initial channels: %+v", channels)
	}
	settings, err := s.Settings(testContext)
	if err != nil {
		t.Fatal(err)
	}
	if settings.DefaultMode != "relay" || settings.DefaultQuality != 720 || settings.MonthlyBudgetGB != 800 || len(settings.PlaybackToken) != 64 {
		t.Fatalf("invalid defaults: %+v", settings)
	}
	settings.BaseURL = "https://tv.example.com/"
	if err := s.SaveSettings(testContext, settings); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTraffic(testContext, "2026-10", 123); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTraffic(testContext, "2026-10", 456); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 60)
	if err := s.SetAdminHash(testContext, hash); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0600 {
		t.Fatalf("database mode = %o", stat.Mode().Perm())
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	reopened, err := s.Channels(testContext)
	if err != nil || !reflect.DeepEqual(channels, reopened) {
		t.Fatalf("channels changed on reopen: %v %+v", err, reopened)
	}
	loaded, err := s.Settings(testContext)
	if err != nil || loaded.BaseURL != "https://tv.example.com" || loaded.PlaybackToken != settings.PlaybackToken {
		t.Fatalf("settings persistence: %+v %v", loaded, err)
	}
	traffic, err := s.Traffic(testContext, "2026-10")
	if err != nil || traffic.Bytes != 579 {
		t.Fatalf("traffic = %+v, %v", traffic, err)
	}
	loadedHash, err := s.AdminHash(testContext)
	if err != nil || loadedHash != hash {
		t.Fatalf("admin hash not persisted: %v", err)
	}
}

func TestStableChannelIDs(t *testing.T) {
	s := testStore(t)
	channels, err := s.Channels(testContext)
	if err != nil {
		t.Fatal(err)
	}
	first, second := channels[0], channels[1]
	if err := s.Reorder(testContext, []string{second.ID, first.ID}); err != nil {
		t.Fatal(err)
	}
	reordered, err := s.Channels(testContext)
	if err != nil || reordered[0].ID != second.ID || reordered[1].ID != first.ID {
		t.Fatalf("unexpected order: %+v %v", reordered, err)
	}
	for _, original := range channels {
		current, err := s.Channel(testContext, original.ID)
		if err != nil || current.URL != original.URL {
			t.Fatalf("playback ID changed target: %+v %v", current, err)
		}
	}
	first.Name = "改名"
	first.SortOrder = 0
	updated, err := s.SaveChannel(testContext, first)
	if err != nil || updated.SortOrder != 1 || updated.ID != first.ID {
		t.Fatalf("editing changed ordering: %+v %v", updated, err)
	}
	if err := s.DeleteChannel(testContext, second.ID); err != nil {
		t.Fatal(err)
	}
	current, err := s.Channel(testContext, first.ID)
	if err != nil || current.URL != first.URL {
		t.Fatalf("deletion changed surviving ID: %+v %v", current, err)
	}
	if _, err := s.Channel(testContext, second.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted ID error = %v", err)
	}
	created, err := s.SaveChannel(testContext, core.Channel{Name: "新增", URL: "https://youtu.be/V1p33hqPrUk", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == second.ID || created.ID == first.ID || created.SortOrder != 2 || created.Proxy != "inherit" {
		t.Fatalf("invalid new channel: %+v", created)
	}
	created.ID = "does-not-exist"
	if _, err := s.SaveChannel(testContext, created); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown channel was upserted: %v", err)
	}
}

func TestReorderIsAtomic(t *testing.T) {
	s := testStore(t)
	before, _ := s.Channels(testContext)
	for _, ids := range [][]string{{before[0].ID}, {before[0].ID, before[0].ID}, {before[1].ID, "unknown"}} {
		if err := s.Reorder(testContext, ids); !errors.Is(err, ErrValidation) {
			t.Errorf("reorder error = %v", err)
		}
		after, err := s.Channels(testContext)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("failed reorder modified state: %+v %v", after, err)
		}
	}
}

func TestImportIsAtomicAndExcludesAdmin(t *testing.T) {
	s := testStore(t)
	hash := strings.Repeat("x", 60)
	if err := s.SetAdminHash(testContext, hash); err != nil {
		t.Fatal(err)
	}
	original, err := s.Export(testContext)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"source", "duplicate-id", "duplicate-order", "settings", "version"} {
		t.Run(bad, func(t *testing.T) {
			backup, _ := s.Export(testContext)
			backup.Channels[0].Name = "would change"
			switch bad {
			case "source":
				backup.Channels[1].URL = "http://127.0.0.1/private"
			case "duplicate-id":
				backup.Channels[1].ID = backup.Channels[0].ID
			case "duplicate-order":
				backup.Channels[1].SortOrder = backup.Channels[0].SortOrder
			case "settings":
				backup.Settings.BaseURL = "https://tv.example.com/subpath"
			case "version":
				backup.Version = 3
			}
			if err := s.Import(testContext, backup); !errors.Is(err, ErrValidation) {
				t.Fatalf("import error = %v", err)
			}
			after, err := s.Export(testContext)
			if err != nil || !reflect.DeepEqual(original, after) {
				t.Fatalf("failed import modified database: %+v %v", after, err)
			}
		})
	}
	imported := original
	imported.Channels = append([]core.Channel(nil), original.Channels...)
	imported.Channels[0].Name = "已导入"
	imported.Settings.PlaybackToken = strings.Repeat("b", 64)
	if err := s.Import(testContext, imported); err != nil {
		t.Fatal(err)
	}
	after, err := s.Export(testContext)
	if err != nil || !reflect.DeepEqual(imported, after) {
		t.Fatalf("backup mismatch: %+v %v", after, err)
	}
	loadedHash, err := s.AdminHash(testContext)
	if err != nil || loadedHash != hash {
		t.Fatalf("import changed administrator hash: %v", err)
	}
}

func TestChannelValidation(t *testing.T) {
	s := testStore(t)
	valid := core.Channel{Name: "正常频道", URL: "https://www.youtube.com/watch?v=vr3XyVCR4T0", Group: "新闻", Enabled: true}
	tests := []struct {
		name   string
		change func(*core.Channel)
	}{
		{"foreign domain", func(ch *core.Channel) { ch.URL = "https://youtube.com.evil.test/watch?v=vr3XyVCR4T0" }},
		{"credentials", func(ch *core.Channel) { ch.URL = "https://user:password@youtube.com/watch?v=vr3XyVCR4T0" }},
		{"local source", func(ch *core.Channel) { ch.URL = "http://127.0.0.1:8000/stream" }},
		{"file source", func(ch *core.Channel) { ch.URL = "file:///etc/passwd" }},
		{"extra video", func(ch *core.Channel) { ch.URL += "&v=V1p33hqPrUk" }},
		{"invalid video", func(ch *core.Channel) { ch.URL = "https://youtu.be/too-short" }},
		{"name newline", func(ch *core.Channel) { ch.Name = "Channel\n#EXTINF:malicious" }},
		{"trailing newline", func(ch *core.Channel) { ch.Name = "Channel\n" }},
		{"group attribute", func(ch *core.Channel) { ch.Group = `news" tvg-id="fake` }},
		{"logo attribute", func(ch *core.Channel) { ch.Logo = `https://example.com/logo" x="bad` }},
		{"mode", func(ch *core.Channel) { ch.Mode = "auto" }},
		{"quality", func(ch *core.Channel) { ch.Quality = 999 }},
		{"proxy path", func(ch *core.Channel) { ch.Proxy = "http://127.0.0.1:8080/path" }},
		{"proxy scheme", func(ch *core.Channel) { ch.Proxy = "file:///etc/passwd" }},
		{"proxy port", func(ch *core.Channel) { ch.Proxy = "http://localhost:99999" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := valid
			tt.change(&ch)
			if _, err := s.SaveChannel(testContext, ch); !errors.Is(err, ErrValidation) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	valid.Proxy = "socks5://user:password@127.0.0.1:1080"
	valid.URL = "https://youtu.be/vr3XyVCR4T0?si=tracking"
	created, err := s.SaveChannel(testContext, valid)
	if err != nil || created.URL != "https://www.youtube.com/watch?v=vr3XyVCR4T0" {
		t.Fatalf("valid proxy or URL rejected: %+v %v", created, err)
	}
}

func TestSettingsValidation(t *testing.T) {
	s := testStore(t)
	original, err := s.Settings(testContext)
	if err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"javascript:alert(1)", "https://user:pass@example.com", "https://tv.example.com/api", "https://tv.example.com?token=a", "https://tv.example.com#fragment", "https://tv.example.com:99999"} {
		settings := original
		settings.BaseURL = base
		if err := s.SaveSettings(testContext, settings); !errors.Is(err, ErrValidation) {
			t.Errorf("accepted base URL %q: %v", base, err)
		}
	}
	settings := original
	settings.PlaybackToken = "short"
	if err := s.SaveSettings(testContext, settings); !errors.Is(err, ErrValidation) {
		t.Fatalf("accepted short token: %v", err)
	}
	settings = original
	settings.UpstreamProxy = "inherit"
	if err := s.SaveSettings(testContext, settings); !errors.Is(err, ErrValidation) {
		t.Fatalf("accepted globally inherited proxy: %v", err)
	}
	after, err := s.Settings(testContext)
	if err != nil || !reflect.DeepEqual(original, after) {
		t.Fatalf("invalid settings changed database: %v", err)
	}
}

func TestTrafficValidation(t *testing.T) {
	s := testStore(t)
	for _, month := range []string{"2026-00", "2026-13", "bad", "2026-1"} {
		if err := s.AddTraffic(testContext, month, 1); !errors.Is(err, ErrValidation) {
			t.Errorf("accepted month %q: %v", month, err)
		}
	}
	if err := s.AddTraffic(testContext, "2026-10", -1); !errors.Is(err, ErrValidation) {
		t.Fatalf("accepted negative traffic: %v", err)
	}
	result, err := s.Traffic(testContext, "2026-11")
	if err != nil || result.Bytes != 0 {
		t.Fatalf("empty traffic = %+v, %v", result, err)
	}
}

func TestConcurrentTrafficDoesNotLoseUpdates(t *testing.T) {
	s := testStore(t)
	var group sync.WaitGroup
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := s.AddTraffic(testContext, "2026-10", 1024); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	traffic, err := s.Traffic(testContext, "2026-10")
	if err != nil || traffic.Bytes != 12*1024 {
		t.Fatalf("concurrent traffic = %+v, %v", traffic, err)
	}
}
