package web

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"iptv-manager/internal/core"
	"iptv-manager/internal/store"
)

func TestProxyAPI(t *testing.T) {
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
	tested := make(chan string, 8)
	release := make(chan struct{})
	var blocking atomic.Bool
	media := &fakeMedia{}
	h, err := New(db, media, Options{SecureCookies: true, ProxyTester: func(ctx context.Context, proxyURL string) (string, error) {
		tested <- proxyURL
		if blocking.Load() {
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		if strings.Contains(proxyURL, "bad.example") {
			return "", errors.New("连接被拒绝")
		}
		return "203.0.113.9", nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := loginAsAdmin(t, h)
	for _, route := range []struct{ method, path string }{{"POST", "/api/proxies"}, {"PUT", "/api/proxies/x"}, {"DELETE", "/api/proxies/x"}, {"POST", "/api/proxies/x/test"}, {"PUT", "/api/providers/tvb/proxy"}} {
		if w := request(h, route.method, route.path, `{}`, nil, "", ""); w.Code != 401 {
			t.Fatalf("%s %s without login: %d", route.method, route.path, w.Code)
		}
		if w := request(h, route.method, route.path, `{}`, cookie, "", ""); w.Code != 403 {
			t.Fatalf("%s %s without CSRF: %d", route.method, route.path, w.Code)
		}
	}
	if len(tested) != 0 {
		t.Fatal("unauthorized proxy test executed")
	}
	w := request(h, "POST", "/api/proxies", `{"name":"家里","scheme":"socks5","host":"10.10.1.38","port":1080,"username":"","password":""}`, cookie, csrf, "")
	var p core.Proxy
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &p) != nil || len(p.ID) != 24 || p.URL() != "socks5://10.10.1.38:1080" {
		t.Fatalf("create proxy: %d %s", w.Code, w.Body.String())
	}
	if w := request(h, "POST", "/api/proxies", `{"name":"","scheme":"socks5","host":"10.10.1.38","port":1080}`, cookie, csrf, ""); w.Code != 400 || !strings.Contains(w.Body.String(), "name") {
		t.Fatalf("invalid proxy: %d %s", w.Code, w.Body.String())
	}
	if w := request(h, "PUT", "/api/proxies/ffffffffffffffffffffffff", `{"name":"x","scheme":"http","host":"h.example","port":1}`, cookie, csrf, ""); w.Code != 404 {
		t.Fatalf("unknown proxy update: %d", w.Code)
	}
	w = request(h, "GET", "/api/state", "", cookie, "", "")
	var state struct{ Settings core.Settings }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &state) != nil || len(state.Settings.Proxies) != 1 || state.Settings.ProviderProxies["tvb"] != core.DirectProxy {
		t.Fatalf("state: %d %s", w.Code, w.Body.String())
	}
	// The settings form cannot drop or alter proxies.
	if w := request(h, "PUT", "/api/settings", `{"base_url":"https://tv.example","default_quality":720,"monthly_budget_gb":800}`, cookie, csrf, ""); w.Code != 200 {
		t.Fatalf("settings: %d %s", w.Code, w.Body.String())
	}
	settings, err := db.Settings(ctx)
	if err != nil || len(settings.Proxies) != 1 || settings.BaseURL != "https://tv.example" || settings.PlaybackToken != state.Settings.PlaybackToken {
		t.Fatalf("settings lost proxies or token: %+v %v", settings, err)
	}
	body, _ := json.Marshal(core.BulkChannelRequest{SourceType: "builtin", ProviderID: "tvb", ChannelIDs: []string{"C"}})
	w = request(h, "POST", "/api/channels/bulk", string(body), cookie, csrf, "")
	var bulk core.BulkChannelResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &bulk) != nil || bulk.Added != 1 {
		t.Fatalf("builtin add: %d %s", w.Code, w.Body.String())
	}
	tvbChannel := bulk.Results[0].ChannelID
	media.invalidated = nil
	if w := request(h, "PUT", "/api/providers/tvb/proxy", `{"proxy":"`+p.ID+`"}`, cookie, csrf, ""); w.Code != 200 {
		t.Fatalf("provider proxy: %d %s", w.Code, w.Body.String())
	}
	if settings, _ = db.Settings(ctx); settings.ProviderProxies["tvb"] != p.ID {
		t.Fatal("provider proxy not saved")
	}
	if len(media.invalidated) != 1 || media.invalidated[0] != tvbChannel {
		t.Fatalf("provider proxy change did not invalidate its channels: %v", media.invalidated)
	}
	if w := request(h, "PUT", "/api/providers/unknown/proxy", `{"proxy":"direct"}`, cookie, csrf, ""); w.Code != 404 {
		t.Fatalf("unknown provider: %d", w.Code)
	}
	if w := request(h, "PUT", "/api/providers/tvb/proxy", `{"proxy":"ffffffffffffffffffffffff"}`, cookie, csrf, ""); w.Code != 400 {
		t.Fatalf("dangling provider proxy: %d", w.Code)
	}
	w = request(h, "POST", "/api/channels", `{"name":"频道","source_type":"stream","url":"https://example.com/live.m3u8","enabled":true,"mode":"relay","proxy":"`+p.ID+`"}`, cookie, csrf, "")
	var ch core.Channel
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &ch) != nil || ch.Proxy != p.ID {
		t.Fatalf("channel with proxy: %d %s", w.Code, w.Body.String())
	}
	if w := request(h, "POST", "/api/channels", `{"name":"频道2","source_type":"stream","url":"https://example.com/2.m3u8","enabled":true,"proxy":"socks5://x.example:1080"}`, cookie, csrf, ""); w.Code != 400 {
		t.Fatalf("URL proxy on channel accepted: %d", w.Code)
	}
	w = request(h, "DELETE", "/api/proxies/"+p.ID, "", cookie, csrf, "")
	var usage struct {
		Error         string
		Channels      int
		Subscriptions int
		Providers     int
	}
	if w.Code != 409 || json.Unmarshal(w.Body.Bytes(), &usage) != nil || usage.Channels != 1 || usage.Providers != 1 || usage.Subscriptions != 0 || !strings.Contains(usage.Error, "1 个频道") {
		t.Fatalf("in-use delete: %d %s", w.Code, w.Body.String())
	}
	media.invalidated = nil
	if w := request(h, "PUT", "/api/proxies/"+p.ID, `{"name":"家里","scheme":"socks5","host":"10.10.1.38","port":1081}`, cookie, csrf, ""); w.Code != 200 {
		t.Fatalf("update proxy: %d %s", w.Code, w.Body.String())
	}
	if len(media.invalidated) < 2 {
		t.Fatalf("proxy edit did not invalidate channels: %v", media.invalidated)
	}
	w = request(h, "POST", "/api/proxies/"+p.ID+"/test", "", cookie, csrf, "")
	var outcome struct {
		OK        bool   `json:"ok"`
		IP        string `json:"ip"`
		ElapsedMS int64  `json:"elapsed_ms"`
		Error     string `json:"error"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &outcome) != nil || !outcome.OK || outcome.IP != "203.0.113.9" {
		t.Fatalf("proxy test: %d %s", w.Code, w.Body.String())
	}
	if used := <-tested; used != "socks5://10.10.1.38:1081" {
		t.Fatalf("tested URL = %s", used)
	}
	if w := request(h, "POST", "/api/proxies/ffffffffffffffffffffffff/test", "", cookie, csrf, ""); w.Code != 404 {
		t.Fatalf("unknown proxy test: %d", w.Code)
	}
	w = request(h, "POST", "/api/proxies", `{"name":"坏代理","scheme":"http","host":"bad.example","port":8080}`, cookie, csrf, "")
	var bad core.Proxy
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &bad) != nil {
		t.Fatalf("create bad proxy: %d %s", w.Code, w.Body.String())
	}
	w = request(h, "POST", "/api/proxies/"+bad.ID+"/test", "", cookie, csrf, "")
	if w.Code != 502 || json.Unmarshal(w.Body.Bytes(), &outcome) != nil || outcome.Error != "连接被拒绝" {
		t.Fatalf("failed proxy test: %d %s", w.Code, w.Body.String())
	}
	<-tested
	// Only one test runs at a time; a second request is rejected, not queued.
	blocking.Store(true)
	var group sync.WaitGroup
	group.Add(1)
	var first int
	go func() {
		defer group.Done()
		first = request(h, "POST", "/api/proxies/"+p.ID+"/test", "", cookie, csrf, "").Code
	}()
	<-tested
	if w := request(h, "POST", "/api/proxies/"+bad.ID+"/test", "", cookie, csrf, ""); w.Code != 409 {
		t.Fatalf("concurrent proxy test: %d %s", w.Code, w.Body.String())
	}
	close(release)
	group.Wait()
	if first != 200 || len(tested) != 0 {
		t.Fatalf("blocked test finished with %d, pending %d", first, len(tested))
	}
	if w := request(h, "DELETE", "/api/proxies/"+bad.ID, "", cookie, csrf, ""); w.Code != 200 {
		t.Fatalf("delete unused proxy: %d %s", w.Code, w.Body.String())
	}
	if w := request(h, "DELETE", "/api/proxies/"+bad.ID, "", cookie, csrf, ""); w.Code != 404 {
		t.Fatalf("delete missing proxy: %d", w.Code)
	}
}
