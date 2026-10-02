package web

import (
	"context"
	"encoding/json"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"iptv-manager/internal/core"
	"iptv-manager/internal/store"
)

func TestBuiltinSourcesImportThroughBulkAPI(t *testing.T) {
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
	handler, err := New(repo, media, Options{SecureCookies: true})
	if err != nil {
		t.Fatal(err)
	}
	if w := request(handler, "GET", "/api/builtin-sources", "", nil, "", ""); w.Code != 401 {
		t.Fatalf("catalog did not require login: %d", w.Code)
	}
	cookie, csrf := loginAsAdmin(t, handler)
	w := request(handler, "GET", "/api/builtin-sources", "", cookie, "", "")
	var catalog struct {
		Sources []struct {
			ID, Name   string
			SourceType string `json:"source_type"`
			Channels   []struct{ ID, Name, URL string }
		}
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &catalog) != nil || len(catalog.Sources) == 0 {
		t.Fatalf("invalid catalog: %d %s", w.Code, w.Body.String())
	}
	for _, source := range catalog.Sources {
		kind := source.SourceType
		if kind == "" {
			kind = "stream"
		}
		if len(source.Channels) == 0 {
			t.Fatalf("empty source: %s", source.ID)
		}
		keys := make([]string, len(source.Channels))
		for i, channel := range source.Channels {
			keys[i] = channel.ID
		}
		body, err := json.Marshal(core.BulkChannelRequest{SourceType: kind, ProviderID: source.ID, ChannelIDs: keys, Group: source.Name, Mode: "direct"})
		if err != nil {
			t.Fatal(err)
		}
		w = request(handler, "POST", "/api/channels/bulk", string(body), cookie, csrf, "")
		var result core.BulkChannelResult
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Added != len(keys) || result.Failed != 0 || result.Skipped != 0 {
			t.Fatalf("builtin import failed: %d %s", w.Code, w.Body.String())
		}
		for i, item := range result.Results {
			channel, err := repo.Channel(ctx, item.ChannelID)
			if err != nil || channel.SourceType != kind || channel.ProviderID != source.ID || channel.ProviderChannelID != source.Channels[i].ID || channel.Mode != "direct" || channel.Proxy != "inherit" || channel.Group != source.Name || channel.Name != source.Channels[i].Name || channel.URL != source.Channels[i].URL {
				t.Fatalf("invalid imported channel: %+v %v", channel, err)
			}
		}
		first, _ := repo.Channel(ctx, result.Results[0].ChannelID)
		first.Mode = "relay"
		if _, err := repo.SaveChannel(ctx, first); err != nil {
			t.Fatal(err)
		}
		w = request(handler, "POST", "/api/channels/bulk", string(body), cookie, csrf, "")
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Added != 0 || result.Skipped != len(keys) || result.Failed != 0 {
			t.Fatalf("duplicate builtin import failed: %d %s", w.Code, w.Body.String())
		}
		preserved, _ := repo.Channel(ctx, first.ID)
		if preserved.Mode != "relay" {
			t.Fatal("duplicate import overwrote playback settings")
		}
	}
	if len(media.invalidated) != 0 {
		t.Fatal("catalog import affected existing streams")
	}
}
