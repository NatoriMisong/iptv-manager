package web

import (
	"context"
	"encoding/json"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"iptv-manager/internal/core"
	"iptv-manager/internal/store"
)

func TestSubscriptionAPI(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hash, _ := bcrypt.GenerateFromPassword([]byte("a-strong-password"), bcrypt.MinCost)
	if err := db.SetAdminHash(ctx, string(hash)); err != nil {
		t.Fatal(err)
	}
	synced := ""
	h, err := New(db, &fakeMedia{}, Options{SecureCookies: true, SyncSubscription: func(_ context.Context, id string) error { synced = id; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := loginAsAdmin(t, h)
	for _, route := range []struct{ method, path string }{{"POST", "/api/subscriptions"}, {"PUT", "/api/subscriptions/test"}, {"DELETE", "/api/subscriptions/test"}, {"POST", "/api/subscriptions/test/sync"}} {
		for _, auth := range []bool{false, true} {
			c := cookie
			if !auth {
				c = nil
			}
			w := request(h, route.method, route.path, `{}`, c, "", "")
			want := 401
			if auth {
				want = 403
			}
			if w.Code != want {
				t.Fatalf("route protection %s: %d", route.path, w.Code)
			}
		}
	}
	if synced != "" {
		t.Fatal("unauthorized sync invoked")
	}
	w := request(h, "POST", "/api/subscriptions", `{"name":"IPTV","url":"http://public.example/channels.m3u","interval_minutes":60,"enabled":true,"proxy":"inherit"}`, cookie, csrf, "")
	var sub core.Subscription
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &sub) != nil || sub.ID == "" {
		t.Fatalf("save subscription: %d %s", w.Code, w.Body.String())
	}
	w = request(h, "POST", "/api/subscriptions/"+sub.ID+"/sync", `{}`, cookie, csrf, "")
	if w.Code != 200 || synced != sub.ID {
		t.Fatal("manual sync not invoked")
	}
	w = request(h, "GET", "/api/state", "", cookie, "", "")
	var state struct{ Subscriptions []core.Subscription }
	if json.Unmarshal(w.Body.Bytes(), &state) != nil || len(state.Subscriptions) != 1 {
		t.Fatal("state missing subscription")
	}
	w = request(h, "POST", "/api/channels", `{"name":"通用直播","source_type":"stream","url":"https://cdn.example/live.m3u8","mode":"direct","enabled":true}`, cookie, csrf, "")
	var ch core.Channel
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &ch) != nil || !ch.IsStream() {
		t.Fatalf("generic channel save: %d %s", w.Code, w.Body.String())
	}
	w = request(h, "POST", "/api/channels/"+ch.ID+"/refresh", `{}`, cookie, csrf, "")
	if w.Code != 200 {
		t.Fatal("generic refresh failed")
	}
	w = request(h, "DELETE", "/api/subscriptions/"+sub.ID, `{}`, cookie, csrf, "")
	if w.Code != 200 {
		t.Fatal("delete subscription failed")
	}
	if _, err := db.Channel(ctx, ch.ID); err != nil {
		t.Fatal("deletion removed manual stream")
	}
}
