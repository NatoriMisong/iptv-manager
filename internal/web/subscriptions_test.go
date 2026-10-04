package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"iptv-manager/internal/core"
	"iptv-manager/internal/source"
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
	for _, route := range []struct{ method, path string }{{"POST", "/api/subscriptions"}, {"PUT", "/api/subscriptions/test"}, {"DELETE", "/api/subscriptions/test"}, {"POST", "/api/subscriptions/test/sync"}, {"POST", "/api/subscriptions/test/clear"}, {"POST", "/api/subscriptions/test/channels"}} {
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
	if w := request(h, "GET", "/api/subscriptions/test/entries", "", nil, "", ""); w.Code != 401 {
		t.Fatalf("entries exposed without login: %d", w.Code)
	}
	w := request(h, "POST", "/api/subscriptions", `{"name":"IPTV","sources":[{"url":"http://public.example/channels.m3u"},{"id":"","url":"http://mirror.example/channels.m3u"}],"interval_minutes":60,"enabled":true,"proxy":"direct"}`, cookie, csrf, "")
	var sub core.Subscription
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &sub) != nil || sub.ID == "" || len(sub.Sources) != 2 || sub.Sources[0].ID == "" {
		t.Fatalf("save subscription: %d %s", w.Code, w.Body.String())
	}
	if w := request(h, "POST", "/api/subscriptions", `{"name":"旧格式","url":"http://public.example/channels.m3u","interval_minutes":60,"enabled":true}`, cookie, csrf, ""); w.Code != 400 {
		t.Fatalf("legacy single-url payload accepted: %d", w.Code)
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
	if _, err := db.ApplySource(ctx, sub, sub.Sources[0].ID, source.Playlist{Entries: []source.Entry{{Name: "A", URL: "https://live.cdn.example/a.m3u8", Group: "新闻"}, {Name: "B", URL: "https://other.example/b.m3u8"}}}); err != nil {
		t.Fatal(err)
	}
	w = request(h, "GET", "/api/subscriptions/"+sub.ID+"/entries", "", cookie, "", "")
	var catalogue struct{ Entries []core.SubscriptionEntry }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &catalogue) != nil || len(catalogue.Entries) != 2 || catalogue.Entries[0].Added {
		t.Fatalf("entries: %d %s", w.Code, w.Body.String())
	}
	if w := request(h, "GET", "/api/subscriptions/missing00000/entries", "", cookie, "", ""); w.Code != 404 {
		t.Fatalf("entries for unknown subscription: %d", w.Code)
	}
	addBody := `{"items":[{"source_id":"` + sub.Sources[0].ID + `","name":"A"},{"source_id":"` + sub.Sources[0].ID + `","name":"B"},{"source_id":"` + sub.Sources[0].ID + `","name":"Z"}],"group":"","defaults":[{"domain":"cdn.example","mode":"direct","proxy":"direct"}]}`
	for _, bad := range []string{`{"items":[]}`, `{"items":[{"source_id":"x","name":"A"}]}`, addBody + `{}`, `{"items":[{"source_id":"` + sub.Sources[0].ID + `","name":"A"}],"extra":1}`} {
		if w := request(h, "POST", "/api/subscriptions/"+sub.ID+"/channels", bad, cookie, csrf, ""); w.Code != 400 {
			t.Fatalf("invalid add accepted %s: %d %s", bad, w.Code, w.Body.String())
		}
	}
	w = request(h, "POST", "/api/subscriptions/"+sub.ID+"/channels", addBody, cookie, csrf, "")
	var added core.BulkChannelResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &added) != nil || added.Added != 2 || added.Failed != 1 {
		t.Fatalf("add from catalogue: %d %s", w.Code, w.Body.String())
	}
	imported := []string{added.Results[0].ChannelID, added.Results[1].ChannelID}
	first, _ := db.Channel(ctx, imported[0])
	second, _ := db.Channel(ctx, imported[1])
	if first.Mode != "direct" || first.Group != "新闻" || second.Mode != "relay" || second.SourceKey != "B" {
		t.Fatalf("defaults by origin: %+v %+v", first, second)
	}
	if w := request(h, "DELETE", "/api/channels/"+imported[1], `{}`, cookie, csrf, ""); w.Code != 200 {
		t.Fatalf("subscription channel not deletable: %d %s", w.Code, w.Body.String())
	}
	w = request(h, "GET", "/api/subscriptions/"+sub.ID+"/entries", "", cookie, "", "")
	if json.Unmarshal(w.Body.Bytes(), &catalogue) != nil || !catalogue.Entries[0].Added || catalogue.Entries[1].Added {
		t.Fatalf("added flags after delete: %s", w.Body.String())
	}
	imported = imported[:1]
	if w = request(h, "POST", "/api/subscriptions/missing00000/clear", `{}`, cookie, csrf, ""); w.Code != 404 {
		t.Fatalf("clear unknown subscription: %d", w.Code)
	}
	w = request(h, "POST", "/api/subscriptions/"+sub.ID+"/clear", `{}`, cookie, csrf, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"deleted":1`) {
		t.Fatalf("clear subscription: %d %s", w.Code, w.Body.String())
	}
	if _, err := db.Subscription(ctx, sub.ID); err != nil {
		t.Fatal("clear removed the subscription itself")
	}
	for _, id := range imported {
		if _, err := db.Channel(ctx, id); err == nil {
			t.Fatal("clear kept an imported channel")
		}
	}
	if _, err := db.Channel(ctx, ch.ID); err != nil {
		t.Fatal("clear removed a manual channel")
	}
	w = request(h, "DELETE", "/api/subscriptions/"+sub.ID, `{}`, cookie, csrf, "")
	if w.Code != 200 {
		t.Fatal("delete subscription failed")
	}
	if _, err := db.Channel(ctx, ch.ID); err != nil {
		t.Fatal("deletion removed manual stream")
	}
}
