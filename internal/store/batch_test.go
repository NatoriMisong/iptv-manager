package store

import (
	"errors"
	"testing"

	"iptv-manager/internal/core"
	"iptv-manager/internal/source"
)

func TestBatchUpdateAndDelete(t *testing.T) {
	s := testStore(t)
	proxy, err := s.SaveProxy(testContext, core.Proxy{Name: "出口", Scheme: "socks5", Host: "proxy.example", Port: 1080})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := s.SaveChannel(testContext, core.Channel{Name: "通用", SourceType: "stream", URL: "https://cdn.example/a.m3u8", Group: "旧组", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	youtube, err := s.SaveChannel(testContext, core.Channel{Name: "油管", URL: "https://www.youtube.com/watch?v=vr3XyVCR4T0", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := s.SaveSubscription(testContext, core.Subscription{Name: "列表", URL: "https://example.com/list.m3u", IntervalMinutes: 60, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := s.ApplySubscription(testContext, sub, source.Playlist{Entries: []source.Entry{{Key: "id:one", Name: "One", URL: "https://cdn.example/one.m3u8", Group: "订阅组"}}})
	if err != nil || len(ids) != 1 {
		t.Fatalf("sync: %v %v", ids, err)
	}
	managed := ids[0]
	all := []string{stream.ID, youtube.ID, managed}
	before, _ := s.Export(testContext)

	group, mode, quality, enabled := "新组", "direct", 480, false
	for _, bad := range []core.BulkChannelUpdate{
		{},
		{IDs: all},
		{IDs: []string{stream.ID, stream.ID}, Mode: &mode},
		{IDs: []string{stream.ID, "missing00000"}, Mode: &mode},
		{IDs: []string{stream.ID, "bad id"}, Mode: &mode},
		{IDs: all, Proxy: strPtr("0123456789abcdef01234567")},
		{IDs: all, Mode: strPtr("auto")},
		{IDs: all, Quality: intPtr(999)},
		{IDs: all, Group: strPtr(`a" tvg-id="b`)},
	} {
		if err := s.UpdateChannels(testContext, bad); !errors.Is(err, ErrValidation) && !errors.Is(err, ErrNotFound) {
			t.Fatalf("accepted %+v: %v", bad, err)
		}
	}
	for _, bad := range [][]string{nil, {stream.ID, "missing00000"}, {stream.ID, managed}} {
		if err := s.DeleteChannels(testContext, bad); !errors.Is(err, ErrValidation) && !errors.Is(err, ErrNotFound) {
			t.Fatalf("delete accepted %v: %v", bad, err)
		}
	}
	after, _ := s.Export(testContext)
	if len(after.Channels) != len(before.Channels) {
		t.Fatal("rejected batch modified data")
	}
	for i := range before.Channels {
		if before.Channels[i] != after.Channels[i] {
			t.Fatalf("rejected batch modified channel: %+v", after.Channels[i])
		}
	}

	if err := s.UpdateChannels(testContext, core.BulkChannelUpdate{IDs: all, Group: &group, Mode: &mode, Quality: &quality, Proxy: &proxy.ID, Enabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	got := map[string]core.Channel{}
	for _, id := range all {
		ch, err := s.Channel(testContext, id)
		if err != nil {
			t.Fatal(err)
		}
		got[id] = ch
		if ch.Mode != "direct" || ch.Enabled || ch.Proxy != proxy.ID {
			t.Fatalf("shared fields not applied: %+v", ch)
		}
	}
	if got[stream.ID].Group != "新组" || got[stream.ID].Quality != 0 || got[stream.ID].Name != "通用" || got[stream.ID].SortOrder != stream.SortOrder {
		t.Fatalf("stream: %+v", got[stream.ID])
	}
	if got[youtube.ID].Group != "新组" || got[youtube.ID].Quality != 480 {
		t.Fatalf("youtube: %+v", got[youtube.ID])
	}
	if got[managed].Group != "订阅组" || got[managed].Name != "One" || got[managed].SubscriptionID != sub.ID || got[managed].SourceKey != "id:one" {
		t.Fatalf("subscription fields overwritten: %+v", got[managed])
	}

	if err := s.DeleteChannels(testContext, []string{stream.ID, youtube.ID}); err != nil {
		t.Fatal(err)
	}
	remaining, _ := s.Channels(testContext)
	if len(remaining) != len(before.Channels)-2 {
		t.Fatalf("remaining: %+v", remaining)
	}
	for _, ch := range remaining {
		if ch.ID == stream.ID || ch.ID == youtube.ID {
			t.Fatalf("channel not deleted: %+v", ch)
		}
	}
	if _, err := s.Channel(testContext, managed); err != nil {
		t.Fatalf("subscription channel lost: %v", err)
	}
}

func strPtr(v string) *string { return &v }
func intPtr(v int) *int       { return &v }
