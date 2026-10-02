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
	for _, key := range []string{"C", "F"} {
		ch, err := s.SaveChannel(testContext, core.Channel{Name: "TVB", SourceType: "builtin", ProviderID: "tvb", ProviderChannelID: key, Enabled: true, Quality: 720})
		if err != nil || !ch.IsBuiltin() || ch.Quality != 0 || !strings.HasPrefix(ch.URL, "https://news.tvb.com/tc/live/") {
			t.Fatalf("builtin save: %+v %v", ch, err)
		}
	}
	for _, ch := range []core.Channel{
		{Name: "bad", SourceType: "builtin", ProviderID: "tvb", ProviderChannelID: "X"},
		{Name: "bad", SourceType: "builtin", ProviderID: "unknown", ProviderChannelID: "C"},
		{Name: "bad", SourceType: "stream", ProviderID: "tvb", ProviderChannelID: "C", URL: "https://news.tvb.com/tc/live/C"},
	} {
		if _, err := s.SaveChannel(testContext, ch); err == nil {
			t.Fatal("invalid provider identity accepted")
		}
	}
	result, err := s.AddChannels(testContext, core.BulkChannelRequest{SourceType: "builtin", ProviderID: "tvb", ChannelIDs: []string{"C", "F"}, Mode: "relay"})
	if err != nil || result.Skipped != 2 || result.Added != 0 {
		t.Fatalf("duplicate import: %+v %v", result, err)
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
		if ch.IsBuiltin() && ch.ProviderID == "tvb" {
			count++
		}
	}
	if count != 2 {
		t.Fatal("TVB config lost in backup round trip")
	}
}
