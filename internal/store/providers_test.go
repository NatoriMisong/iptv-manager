package store

import (
	"reflect"
	"testing"

	"iptv-manager/internal/core"
)

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
