package provider

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"iptv-manager/internal/core"
	"iptv-manager/internal/source"
)

func TestRegisteredCatalogAndFixedSourceContract(t *testing.T) {
	ids := map[string]bool{}
	runtime := New(Options{ClientFactory: func(string) (*http.Client, error) { t.Fatal("static source attempted extraction"); return nil, nil }})
	for _, c := range Catalogs() {
		if c.ID == "" || ids[c.ID] || c.SourceType != "builtin" || len(c.Channels) == 0 || c.LinkLabel == "" || c.PlaybackHelp == "" {
			t.Fatal("incomplete or duplicate catalog")
		}
		ids[c.ID] = true
		keys := map[string]bool{}
		for _, item := range c.Channels {
			if keys[item.ID] || item.ID == "" || source.ValidateURL(item.URL) != nil {
				t.Fatal("invalid catalog channel")
			}
			keys[item.ID] = true
			if c.ID == "tvb" || c.ID == "hoy" {
				continue
			}
			ch := core.Channel{SourceType: "builtin", ProviderID: c.ID, ProviderChannelID: item.ID, URL: "https://untrusted.example/ignored"}
			p, err := runtime.Resolve(context.Background(), ch, core.Settings{}, "test")
			if err != nil || p.Session != nil || p.URL != item.URL {
				t.Fatalf("static resolve: %+v %v", p, err)
			}
			if c.ID == "hkstv" && !strings.HasPrefix(p.Headers["User-Agent"], "Mozilla/") {
				t.Fatal("HKSTV request rule lost")
			}
		}
	}
}

func TestLegacyMatchingIsExactAndDoesNotClaimSubscriptions(t *testing.T) {
	for _, raw := range []string{"https://news.tvb.com/en/live/F/", "https://news.tvb.com/tc/live/C"} {
		ch := UpgradeLegacy(core.Channel{SourceType: "tvb", URL: raw})
		if !ch.IsBuiltin() || ch.ProviderID != "tvb" || ch.URL != raw {
			t.Fatal("legacy TVB identity lost")
		}
	}
	for _, raw := range []string{"https://news.tvb.com.evil.test/tc/live/C", "https://news.tvb.com/tc/live/C?key=secret", "https://news.tvb.com/tc/live/%43", "https://news.tvb.com:443/tc/live/C", "http://news.tvb.com/tc/live/C"} {
		if ch := UpgradeLegacy(core.Channel{SourceType: "tvb", URL: raw}); ch.ProviderID != "" {
			t.Fatal("inexact legacy TVB match")
		}
	}
	item, _ := ChannelInfo("hkstv", "mutfysrq")
	for _, ch := range []core.Channel{{SourceType: "stream", URL: item.URL + "?custom=1"}, {SourceType: "stream", URL: item.URL, SubscriptionID: "managed"}, {SourceType: "youtube", URL: item.URL}} {
		if got := UpgradeLegacy(ch); got != ch {
			t.Fatal("unrelated source converted")
		}
	}
}
