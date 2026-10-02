package store

import (
	"strings"
	"testing"

	"iptv-manager/internal/core"
)

func TestTVBChannelValidationBulkAndBackup(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, raw := range []string{"https://news.tvb.com/tc/live/C", "https://news.tvb.com/en/live/F/"} {
		ch, err := s.SaveChannel(testContext, core.Channel{Name: "TVB", SourceType: "tvb", URL: raw, Enabled: true, Quality: 720})
		if err != nil || !ch.IsTVB() || ch.Quality != 0 || !strings.HasPrefix(ch.URL, "https://news.tvb.com/tc/live/") {
			t.Fatalf("TVB save: %+v %v", ch, err)
		}
	}
	for _, raw := range []string{"https://news.tvb.com/tc/live/X", "https://news.tvb.com.evil.test/tc/live/C", "http://news.tvb.com/tc/live/C", "https://user:pass@news.tvb.com/tc/live/C", "https://news.tvb.com:443/tc/live/C", "https://news.tvb.com/tc/live/C?hdnea=secret", "https://news.tvb.com/tc/live/%43"} {
		if _, err := s.SaveChannel(testContext, core.Channel{Name: "bad", SourceType: "tvb", URL: raw}); err == nil {
			t.Fatalf("invalid TVB page accepted: %s", raw)
		}
	}
	result, err := s.AddChannels(testContext, core.BulkChannelRequest{SourceType: "tvb", Text: "无线新闻,https://news.tvb.com/tc/live/C\n无线财经,https://news.tvb.com/tc/live/F", Mode: "relay"})
	if err != nil || result.Skipped != 2 || result.Added != 0 {
		t.Fatalf("TVB duplicate import: %+v %v", result, err)
	}
	backup, err := s.Export(testContext)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Import(testContext, backup); err != nil {
		t.Fatal(err)
	}
	channels, err := s.Channels(testContext)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, ch := range channels {
		if ch.IsTVB() {
			count++
		}
	}
	if count != 2 {
		t.Fatal("TVB config lost in backup round trip")
	}
}
