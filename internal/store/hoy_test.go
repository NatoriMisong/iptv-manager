package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"iptv-manager/internal/core"
)

func TestHOYBuiltinDefaultsDeduplicationAndBackup(t *testing.T) {
	s := testStore(t)
	req := core.BulkChannelRequest{SourceType: "builtin", ProviderID: "hoy", ChannelIDs: []string{"76", "77", "78"}}
	result, err := s.AddChannels(testContext, req)
	if err != nil || result.Added != 3 {
		t.Fatalf("HOY import failed: %+v %v", result, err)
	}
	for i, item := range result.Results {
		ch, err := s.Channel(testContext, item.ChannelID)
		if err != nil || ch.ProviderID != "hoy" || ch.ProviderChannelID != req.ChannelIDs[i] || ch.URL != "https://hoy.tv/live?channel_no="+req.ChannelIDs[i] || ch.Mode != "relay" || ch.Proxy != core.DirectProxy || ch.Group != "HOY" || !ch.Enabled {
			t.Fatalf("HOY defaults lost: %+v %v", ch, err)
		}
		ch.Name = "自定义 " + ch.ProviderChannelID
		ch.Mode, ch.Proxy = "direct", "direct"
		if _, err = s.SaveChannel(testContext, ch); err != nil {
			t.Fatal(err)
		}
	}
	before, err := s.Export(testContext)
	if err != nil {
		t.Fatal(err)
	}
	result, err = s.AddChannels(testContext, req)
	if err != nil || result.Skipped != 3 || result.Added != 0 {
		t.Fatal("HOY duplicate import changed channels")
	}
	encoded, err := json.Marshal(before)
	if err != nil || strings.Contains(string(encoded), "CloudFront") || strings.Contains(string(encoded), "live-stream.hoy.tv") {
		t.Fatal("temporary source exported")
	}
	if err = s.Import(testContext, before); err != nil {
		t.Fatal(err)
	}
	after, err := s.Export(testContext)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("HOY identities/settings lost in backup")
	}
	req.ChannelIDs = []string{"76", "79"}
	if _, err = s.AddChannels(testContext, req); err == nil {
		t.Fatal("unsupported HOY channel accepted")
	}
}
