package subscription

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"iptv-manager/internal/core"
	"iptv-manager/internal/store"
)

func TestSyncSchedulingFailureAndRecovery(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sub, err := db.SaveSubscription(ctx, core.Subscription{Name: "Test", Sources: []core.SubscriptionSource{{URL: "https://provider.example/list"}, {URL: "https://mirror.example/list"}}, IntervalMinutes: 60, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	mode := map[string]string{}
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch mode[r.Header.Get("X-Origin-Host")] {
		case "failure":
			http.Error(w, "secret", 403)
		case "empty":
			fmt.Fprint(w, "#EXTM3U\n")
		case "changed":
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:-1,One\nhttps://cdn.example/live-v2.m3u8\n")
		default:
			fmt.Fprint(w, "#EXTM3U\n#EXTINF:-1 tvg-id=\"one\",One\nhttps://cdn.example/live.m3u8\n")
		}
	}))
	defer upstream.Close()
	invalidated := []string{}
	m := New(db, func(id string) { invalidated = append(invalidated, id) })
	m.client = func(string) (*http.Client, error) {
		return &http.Client{Transport: rewriteTransport{target: upstream.URL, base: upstream.Client().Transport}}, nil
	}
	m.syncDue(ctx)
	if calls != 2 || len(invalidated) != 0 {
		t.Fatalf("initial schedule: %d %v", calls, invalidated)
	}
	if channels, _ := db.Channels(ctx); len(channels) != 2 {
		t.Fatalf("sync created channels: %+v", channels)
	}
	entries, _ := db.SubscriptionEntries(ctx, sub.ID)
	if len(entries) != 2 {
		t.Fatalf("catalogue: %+v", entries)
	}
	m.syncDue(ctx)
	if calls != 2 {
		t.Fatal("scheduler ignored interval")
	}
	primary := sub.Sources[0].ID
	picked, err := db.AddSubscriptionChannels(ctx, sub.ID, core.SubscriptionAddRequest{Items: []core.SubscriptionAddItem{{SourceID: primary, Name: "One"}}})
	if err != nil || picked.Added != 1 {
		t.Fatalf("pick: %+v %v", picked, err)
	}
	before, _ := db.Channels(ctx)
	mode["provider.example"] = "failure"
	if err := m.Sync(ctx, sub.ID); err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "provider.example") {
		t.Fatalf("failure: %v", err)
	}
	after, _ := db.Channels(ctx)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed fetch changed channels")
	}
	failed, _ := db.Subscription(ctx, sub.ID)
	if failed.LastError == "" || failed.LastSync.IsZero() || failed.Sources[0].LastError == "" || failed.Sources[0].Entries != 1 || failed.Sources[1].LastError != "" {
		t.Fatalf("missing per-source diagnostic: %+v", failed)
	}
	mode["provider.example"] = "empty"
	if m.Sync(ctx, sub.ID) == nil {
		t.Fatal("empty list accepted")
	}
	mode["provider.example"] = "changed"
	invalidated = invalidated[:0]
	if err := m.Sync(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	recovered, _ := db.Subscription(ctx, sub.ID)
	if recovered.LastError != "" || recovered.Sources[0].LastError != "" {
		t.Fatal("error did not clear")
	}
	refreshed, _ := db.Channel(ctx, picked.Results[0].ChannelID)
	if refreshed.URL != "https://cdn.example/live-v2.m3u8" || len(invalidated) != 1 {
		t.Fatalf("picked channel not refreshed: %+v %v", refreshed, invalidated)
	}
	recovered.Enabled = false
	if _, err := db.SaveSubscription(ctx, recovered); err != nil {
		t.Fatal(err)
	}
	callsBefore := calls
	m.syncDue(ctx)
	if calls != callsBefore {
		t.Fatal("paused subscription auto-refreshed")
	}
	if err := m.Sync(ctx, sub.ID); err != nil {
		t.Fatal("paused manual refresh rejected")
	}
	m.gate <- struct{}{}
	if m.Sync(ctx, sub.ID) != ErrBusy {
		t.Fatal("concurrent fetch gate missing")
	}
	<-m.gate
}

type rewriteTransport struct {
	target string
	base   http.RoundTripper
}

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("X-Origin-Host", req.URL.Host)
	u := *req.URL
	local, _ := http.NewRequest(http.MethodGet, t.target, nil)
	u.Scheme, u.Host = local.URL.Scheme, local.URL.Host
	clone.URL = &u
	return t.base.RoundTrip(clone)
}
