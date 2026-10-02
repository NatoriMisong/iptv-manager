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
	sub, err := db.SaveSubscription(ctx, core.Subscription{Name: "Test", URL: "https://provider.example/list", IntervalMinutes: 60, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	mode := "valid"
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if mode == "failure" {
			http.Error(w, "secret", 403)
			return
		}
		if mode == "empty" {
			fmt.Fprint(w, "#EXTM3U\n")
			return
		}
		fmt.Fprint(w, "#EXTM3U\n#EXTINF:-1 tvg-id=\"one\",One\nhttps://cdn.example/live.m3u8\n")
	}))
	defer upstream.Close()
	invalidated := []string{}
	m := New(db, func(id string) { invalidated = append(invalidated, id) })
	m.client = func(string) (*http.Client, error) {
		return &http.Client{Transport: rewriteTransport{target: upstream.URL, base: upstream.Client().Transport}}, nil
	}
	m.syncDue(ctx)
	if calls != 1 || len(invalidated) != 1 {
		t.Fatalf("initial schedule: %d %v", calls, invalidated)
	}
	m.syncDue(ctx)
	if calls != 1 {
		t.Fatal("scheduler ignored interval")
	}
	before, _ := db.Channels(ctx)
	mode = "failure"
	if err := m.Sync(ctx, sub.ID); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("failure: %v", err)
	}
	after, _ := db.Channels(ctx)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed fetch changed channels")
	}
	failed, _ := db.Subscription(ctx, sub.ID)
	if failed.LastError == "" || failed.LastSync.IsZero() {
		t.Fatal("missing persisted diagnostic")
	}
	mode = "empty"
	if m.Sync(ctx, sub.ID) == nil {
		t.Fatal("empty list accepted")
	}
	mode = "valid"
	if err := m.Sync(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	recovered, _ := db.Subscription(ctx, sub.ID)
	if recovered.LastError != "" {
		t.Fatal("error did not clear")
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
	u := *req.URL
	local, _ := http.NewRequest(http.MethodGet, t.target, nil)
	u.Scheme, u.Host = local.URL.Scheme, local.URL.Host
	clone.URL = &u
	return t.base.RoundTrip(clone)
}
