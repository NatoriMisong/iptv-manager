package web

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"youtube-tv/internal/core"
	"youtube-tv/internal/store"
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
