package web

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"iptv-manager/internal/core"
	"iptv-manager/internal/store"
)

func TestBulkAPIWithSQLite(t *testing.T) {
	ctx := context.Background()
	repo, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	hash, err := bcrypt.GenerateFromPassword([]byte("a-strong-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetAdminHash(ctx, string(hash)); err != nil {
		t.Fatal(err)
	}
	media := &fakeMedia{}
	h, err := New(repo, media, Options{SecureCookies: true})
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := loginAsAdmin(t, h)
	before, _ := repo.Export(ctx)
	body := `{"text":"测试频道,https://youtu.be/AAAAAAAAAAA\nhttps://youtu.be/vr3XyVCR4T0\nbad-url","group":"新闻","mode":"relay","quality":720}`
	for _, tc := range []struct{ csrf, origin string }{{"", "https://tv.example"}, {csrf, "https://evil.example"}} {
		w := request(h, "POST", "/api/channels/bulk", body, cookie, tc.csrf, tc.origin)
		if w.Code != 403 {
			t.Fatalf("unprotected bulk endpoint: %d", w.Code)
		}
	}
	for _, bad := range []string{
		`{"text":"https://youtu.be/AAAAAAAAAAA","id":"injected"}`,
		body + `{}`,
		`{"text":""}`,
		`{"text":"https://youtu.be/AAAAAAAAAAA","quality":999}`,
		`{"text":"` + strings.Repeat("a", 256<<10+1) + `"}`,
	} {
		w := request(h, "POST", "/api/channels/bulk", bad, cookie, csrf, "")
		if w.Code != 400 {
			t.Fatalf("invalid request: %d", w.Code)
		}
	}
	unchanged, _ := repo.Export(ctx)
	if !reflect.DeepEqual(before, unchanged) {
		t.Fatal("rejected request modified data")
	}
	w := request(h, "POST", "/api/channels/bulk", body, cookie, csrf, "https://tv.example")
	var result core.BulkChannelResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Added != 1 || result.Skipped != 1 || result.Failed != 1 {
		t.Fatalf("bulk response: %d %s", w.Code, w.Body.String())
	}
	ch, err := repo.Channel(ctx, result.Results[0].ChannelID)
	if err != nil || ch.Name != "测试频道" || ch.Group != "新闻" || ch.Mode != "relay" || ch.Quality != 720 || !ch.Enabled {
		t.Fatalf("bulk channel not saved: %+v %v", ch, err)
	}
	w = request(h, "GET", "/api/state", "", cookie, "", "")
	var state struct{ Channels []core.Channel }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &state) != nil || len(state.Channels) != 3 || state.Channels[2].ID != ch.ID {
		t.Fatalf("state missing additions: %d %s", w.Code, w.Body.String())
	}
	if len(media.invalidated) != 0 {
		t.Fatal("bulk creation invalidated existing streams")
	}
}

func TestBulkDatabaseErrorDoesNotLeakDetails(t *testing.T) {
	h, repo, _ := setup(t)
	cookie, csrf := loginAsAdmin(t, h)
	repo.bulkErr = errors.New("private database path and credentials")
	w := request(h, "POST", "/api/channels/bulk", `{"text":"https://youtu.be/AAAAAAAAAAA"}`, cookie, csrf, "")
	if w.Code != 500 || strings.Contains(w.Body.String(), "private") || !strings.Contains(w.Body.String(), "本批次未写入") {
		t.Fatalf("unsafe database error: %d %s", w.Code, w.Body.String())
	}
}

func TestBatchEditAndDeleteAPI(t *testing.T) {
	ctx := context.Background()
	repo, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	hash, err := bcrypt.GenerateFromPassword([]byte("a-strong-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetAdminHash(ctx, string(hash)); err != nil {
		t.Fatal(err)
	}
	media := &fakeMedia{}
	h, err := New(repo, media, Options{SecureCookies: true})
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := loginAsAdmin(t, h)
	channels, _ := repo.Channels(ctx)
	if len(channels) < 2 {
		t.Fatalf("expected seeded channels, got %d", len(channels))
	}
	ids := `["` + channels[0].ID + `","` + channels[1].ID + `"]`
	for _, tc := range []struct{ csrf, origin string }{{"", "https://tv.example"}, {csrf, "https://evil.example"}} {
		for _, path := range []string{"/api/channels/bulk-update", "/api/channels/bulk-delete"} {
			if w := request(h, "POST", path, `{"ids":`+ids+`,"enabled":false}`, cookie, tc.csrf, tc.origin); w.Code != 403 {
				t.Fatalf("unprotected %s: %d", path, w.Code)
			}
		}
	}
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"ids":` + ids + `,"name":"x"}`, 400},
		{`{"ids":` + ids + `}`, 400},
		{`{"ids":[],"enabled":false}`, 400},
		{`{"ids":["` + channels[0].ID + `","missing00000"],"enabled":false}`, 404},
		{`{"ids":` + ids + `,"mode":"auto"}`, 400},
	} {
		if w := request(h, "POST", "/api/channels/bulk-update", tc.body, cookie, csrf, "https://tv.example"); w.Code != tc.code {
			t.Fatalf("bulk-update %s: %d %s", tc.body, w.Code, w.Body.String())
		}
	}
	if len(media.invalidated) != 0 {
		t.Fatal("rejected batch invalidated streams")
	}
	w := request(h, "POST", "/api/channels/bulk-update", `{"ids":`+ids+`,"enabled":false,"mode":"direct","group":"合并"}`, cookie, csrf, "https://tv.example")
	if w.Code != 200 {
		t.Fatalf("bulk-update: %d %s", w.Code, w.Body.String())
	}
	for _, id := range []string{channels[0].ID, channels[1].ID} {
		ch, err := repo.Channel(ctx, id)
		if err != nil || ch.Enabled || ch.Mode != "direct" || ch.Group != "合并" {
			t.Fatalf("not updated: %+v %v", ch, err)
		}
	}
	if len(media.invalidated) != 2 {
		t.Fatalf("invalidated %v", media.invalidated)
	}
	if w := request(h, "POST", "/api/channels/bulk-delete", `{"ids":["`+channels[0].ID+`"],"extra":1}`, cookie, csrf, "https://tv.example"); w.Code != 400 {
		t.Fatalf("strict json: %d", w.Code)
	}
	w = request(h, "POST", "/api/channels/bulk-delete", `{"ids":`+ids+`}`, cookie, csrf, "https://tv.example")
	if w.Code != 200 {
		t.Fatalf("bulk-delete: %d %s", w.Code, w.Body.String())
	}
	remaining, _ := repo.Channels(ctx)
	if len(remaining) != len(channels)-2 || len(media.invalidated) != 4 {
		t.Fatalf("remaining %d invalidated %v", len(remaining), media.invalidated)
	}
}
