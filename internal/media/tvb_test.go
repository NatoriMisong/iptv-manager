package media

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iptv-manager/internal/core"
	"iptv-manager/internal/provider"
)

type tvbTransport func(*http.Request) (*http.Response, error)

func (f tvbTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type tvbFixture struct {
	t            *testing.T
	mu           sync.Mutex
	api          map[string]int
	media        map[string]int
	clock        atomic.Int64
	deny         atomic.Bool
	denyFirst    atomic.Bool
	expireCookie atomic.Bool
}

func (f *tvbFixture) now() time.Time        { return time.Unix(f.clock.Load(), 0) }
func (f *tvbFixture) calls(code string) int { f.mu.Lock(); defer f.mu.Unlock(); return f.api[code] }

func (f *tvbFixture) request(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Hostname() == "news.tvb.com" {
		code := strings.TrimPrefix(r.URL.Path, "/app/public/live/stream/")
		if r.Method != "POST" || (code != "C" && code != "F") {
			f.t.Errorf("unexpected TVB API request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Cookie") != "" {
			f.t.Error("CDN cookies leaked to API")
		}
		f.api[code]++
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"channel_id": code, "protocol": "hls", "stream_url": fmt.Sprintf("https://cdn.tvb.com/%s/%d/master.m3u8?signature=secret", code, f.api[code]), "expire_time": f.now().Add(time.Hour).Unix(), "refresh_interval": 3600, "header_json": map[string]string{"Cookie": "do-not-use", "Authorization": "do-not-use"}}})
	} else if r.URL.Hostname() == "keys.tvb.com" {
		if r.Header.Get("Cookie") != "" {
			f.t.Error("host-only Cookie leaked to key host")
		}
		fmt.Fprint(w, "0123456789abcdef")
	} else if r.URL.Hostname() == "cdn.tvb.com" {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(parts) != 3 {
			f.t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		code, generation, file := parts[0], parts[1], parts[2]
		want := code + "-" + generation
		if r.Header.Get("Referer") != "https://news.tvb.com/tc/live/"+code || r.Header.Get("Origin") != "https://news.tvb.com" || r.Header.Get("User-Agent") == "" {
			f.t.Error("TVB request headers lost")
		}
		if r.Header.Get("Authorization") != "" {
			f.t.Error("API response headers replayed")
		}
		cookie, _ := r.Cookie("hdntl")
		if file == "master.m3u8" {
			if cookie != nil && cookie.Value != want {
				f.t.Errorf("session cookies mixed: got %s want %s", cookie.Value, want)
			}
			age := 3600
			if f.expireCookie.Load() {
				age = -1
			}
			http.SetCookie(w, &http.Cookie{Name: "hdntl", Value: want, Path: "/", Secure: true, MaxAge: age})
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"main\",NAME=\"Chinese\",DEFAULT=YES,URI=\"audio.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720,CODECS=\"avc1,mp4a\",AUDIO=\"main\"\nvideo.m3u8\n")
		} else if cookie == nil || cookie.Value != want || f.deny.Load() || (f.denyFirst.Load() && generation == "1") {
			w.WriteHeader(403)
		} else if strings.HasSuffix(file, ".m3u8") {
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-KEY:METHOD=AES-128,URI=\"https://keys.tvb.com/%s/key.bin\"\n#EXTINF:6,\n%s.ts\n", code, strings.TrimSuffix(file, ".m3u8"))
		} else {
			f.media[r.URL.Path]++
			fmt.Fprint(w, "payload "+r.URL.Path)
		}
	} else {
		f.t.Errorf("foreign request: %s", r.URL.Hostname())
		w.WriteHeader(500)
	}
	resp := w.Result()
	resp.Request = r
	return resp, nil
}

func setupTVB(t *testing.T) (*Server, *httptest.Server, *integrationRepo, *tvbFixture) {
	t.Helper()
	f := &tvbFixture{t: t, api: map[string]int{}, media: map[string]int{}}
	f.clock.Store(time.Now().Unix())
	repo := &integrationRepo{settings: core.Settings{PlaybackToken: "tvb-test"}, traffic: map[string]int64{}}
	for _, code := range []string{"C", "F"} {
		repo.channels = append(repo.channels, core.Channel{ID: code, SourceType: "builtin", ProviderID: "tvb", ProviderChannelID: code, URL: "https://news.tvb.com/tc/live/" + code, Enabled: true, Mode: "relay"})
	}
	srv := New(repo, &integrationResolver{}, Options{CacheBytes: 1 << 20, ProviderOptions: provider.Options{Now: f.now, ClientFactory: func(string) (*http.Client, error) { return &http.Client{Transport: tvbTransport(f.request)}, nil }}})
	local := httptest.NewServer(srv.Handler())
	repo.settings.BaseURL = local.URL
	t.Cleanup(func() { local.Close(); srv.providers.Invalidate("C"); srv.providers.Invalidate("F") })
	return srv, local, repo, f
}

func tvbWatch(local *httptest.Server, code string) string {
	return local.URL + "/watch/" + code + "?token=tvb-test"
}

func TestTVBRelayCookieIsolationCachingAndDirect(t *testing.T) {
	_, local, repo, f := setupTVB(t)
	for _, code := range []string{"C", "F", "C"} {
		resp, master := integrationGet(t, local.Client(), tvbWatch(local, code), nil)
		if resp.StatusCode != 200 || strings.Contains(master, "secret") || strings.Contains(master, "cdn.tvb.com") || resp.Header.Get("Set-Cookie") != "" {
			t.Fatalf("bad TVB master: %d %s", resp.StatusCode, master)
		}
		links := integrationLinks(master)
		if len(links) != 2 {
			t.Fatal("missing audio/video")
		}
		for _, link := range links {
			resp, playlist := integrationGet(t, local.Client(), link, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("track: %d %s", resp.StatusCode, playlist)
			}
			for _, part := range integrationLinks(playlist) {
				resp, body := integrationGet(t, local.Client(), part, nil)
				if resp.StatusCode != 200 || (body != "0123456789abcdef" && !strings.HasPrefix(body, "payload /"+code+"/")) {
					t.Fatalf("resource: %d %s", resp.StatusCode, body)
				}
				resp, _ = integrationGet(t, local.Client(), part, nil)
				if resp.StatusCode != 200 {
					t.Fatal("cached resource failed")
				}
			}
		}
	}
	if f.calls("C") != 1 || f.calls("F") != 1 {
		t.Fatal("cached playback re-ran the API")
	}
	client := *local.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	repo.mu.Lock()
	repo.channels[1].Mode = "direct"
	repo.mu.Unlock()
	resp, _ := integrationGet(t, &client, tvbWatch(local, "F"), nil)
	if resp.StatusCode != 307 || !strings.HasPrefix(resp.Header.Get("Location"), "https://cdn.tvb.com/F/") || resp.Header.Get("Set-Cookie") != "" || f.calls("F") != 1 {
		t.Fatal("direct source/cookie handling incorrect")
	}
}

func TestTVBManualRefreshRevokesCookiesResourcesAndCache(t *testing.T) {
	srv, local, _, f := setupTVB(t)
	_, master := integrationGet(t, local.Client(), tvbWatch(local, "C"), nil)
	track := integrationLinks(master)[1]
	_, playlist := integrationGet(t, local.Client(), track, nil)
	segment := integrationLinks(playlist)[1]
	_, _ = integrationGet(t, local.Client(), segment, nil)
	old := currentTVBSession(t, srv)
	srv.Invalidate("C")
	if old.Active() || old.CanRefresh() {
		t.Fatal("old session not revoked")
	}
	for _, raw := range []string{track, segment} {
		resp, _ := integrationGet(t, local.Client(), raw, nil)
		if resp.StatusCode != 410 {
			t.Fatalf("old resource survives refresh: %d", resp.StatusCode)
		}
	}
	if f.calls("C") != 1 || srv.cache.used != 0 {
		t.Fatal("manual clear fetched a new source or retained cache")
	}
	resp, _ := integrationGet(t, local.Client(), tvbWatch(local, "C"), nil)
	if resp.StatusCode != 200 || f.calls("C") != 2 {
		t.Fatal("next playback did not get a new session")
	}
}

func TestTVBExpiryRefreshesAudioAndVideoOnDemand(t *testing.T) {
	for _, cookieExpiry := range []bool{false, true} {
		t.Run(strconv.FormatBool(cookieExpiry), func(t *testing.T) {
			_, local, _, f := setupTVB(t)
			_, master := integrationGet(t, local.Client(), tvbWatch(local, "C"), nil)
			if cookieExpiry {
				f.expireCookie.Store(true)
				resp, _ := integrationGet(t, local.Client(), tvbWatch(local, "C"), nil)
				if resp.StatusCode != 200 {
					t.Fatal("cookie expiration fixture failed")
				}
				f.expireCookie.Store(false)
			} else {
				f.clock.Add(3601)
			}
			if f.calls("C") != 1 {
				t.Fatal("background refresh unexpectedly ran")
			}
			for _, link := range integrationLinks(master) {
				resp, playlist := integrationGet(t, local.Client(), link, nil)
				if resp.StatusCode != 200 {
					t.Fatalf("expired track failed: %d %s", resp.StatusCode, playlist)
				}
				resp, body := integrationGet(t, local.Client(), integrationLinks(playlist)[1], nil)
				if resp.StatusCode != 200 || !strings.HasPrefix(body, "payload /C/2/") {
					t.Fatalf("wrong refreshed track: %d %s", resp.StatusCode, body)
				}
			}
			if f.calls("C") != 2 {
				t.Fatal("audio/video refreshed source more than once")
			}
		})
	}
}

func TestTVBConcurrentFirstPlaybackSharesOneAPIRequest(t *testing.T) {
	_, local, _, f := setupTVB(t)
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			resp, err := local.Client().Get(tvbWatch(local, "C"))
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Errorf("playback status %d", resp.StatusCode)
			}
		}()
	}
	group.Wait()
	if f.calls("C") != 1 {
		t.Fatal("concurrent requests did not share source resolution")
	}
}

func TestTVBPersistent403DoesNotLoopAndManualRefreshUnblocks(t *testing.T) {
	srv, local, _, f := setupTVB(t)
	_, master := integrationGet(t, local.Client(), tvbWatch(local, "C"), nil)
	track := integrationLinks(master)[0]
	f.deny.Store(true)
	resp, _ := integrationGet(t, local.Client(), track, nil)
	if resp.StatusCode != 502 || f.calls("C") != 2 {
		t.Fatal("failed playlist was not retried once")
	}
	for i := 0; i < 3; i++ {
		resp, _ := integrationGet(t, local.Client(), tvbWatch(local, "C"), nil)
		if resp.StatusCode != 503 {
			t.Fatal("missing backoff")
		}
	}
	if f.calls("C") != 2 {
		t.Fatal("403 retry storm reached API")
	}
	f.deny.Store(false)
	srv.Invalidate("C")
	resp, _ = integrationGet(t, local.Client(), tvbWatch(local, "C"), nil)
	if resp.StatusCode != 200 || f.calls("C") != 3 {
		t.Fatal("manual refresh did not unblock")
	}
}

func TestTVB403RenewsBothTracksWithoutReopening(t *testing.T) {
	_, local, _, f := setupTVB(t)
	_, master := integrationGet(t, local.Client(), tvbWatch(local, "C"), nil)
	f.denyFirst.Store(true)
	for _, link := range integrationLinks(master) {
		resp, playlist := integrationGet(t, local.Client(), link, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("track did not recover: %d %s", resp.StatusCode, playlist)
		}
		resp, body := integrationGet(t, local.Client(), integrationLinks(playlist)[1], nil)
		if resp.StatusCode != 200 || !strings.Contains(body, "/C/2/") {
			t.Fatalf("incorrect recovered media: %d %s", resp.StatusCode, body)
		}
	}
	if f.calls("C") != 2 {
		t.Fatal("tracks did not share the renewed session")
	}
}

func TestTVBProxyChangeReplacesSession(t *testing.T) {
	srv, local, repo, f := setupTVB(t)
	var proxies []string
	srv.providers = provider.New(provider.Options{Now: f.now, ClientFactory: func(proxy string) (*http.Client, error) {
		proxies = append(proxies, proxy)
		return &http.Client{Transport: tvbTransport(f.request)}, nil
	}})
	_, master := integrationGet(t, local.Client(), tvbWatch(local, "C"), nil)
	old := currentTVBSession(t, srv)
	repo.settings.Proxies = []core.Proxy{{ID: "0123456789abcdef01234567", Name: "测试代理", Scheme: "socks5", Host: "proxy.example", Port: 1080}}
	repo.settings.ProviderProxies = map[string]string{"tvb": "0123456789abcdef01234567"}
	resp, _ := integrationGet(t, local.Client(), tvbWatch(local, "C"), nil)
	if resp.StatusCode != 200 || f.calls("C") != 2 || len(proxies) != 2 || proxies[1] != "socks5://proxy.example:1080" || old.Active() {
		t.Fatal("proxy change reused the old session")
	}
	resp, _ = integrationGet(t, local.Client(), integrationLinks(master)[0], nil)
	if resp.StatusCode != 410 {
		t.Fatal("old proxy reference still accepted")
	}
}

func currentTVBSession(t *testing.T, srv *Server) provider.Session {
	t.Helper()
	ch, err := srv.repo.Channel(context.Background(), "C")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := srv.repo.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	playback, err := srv.providers.Resolve(context.Background(), ch, settings, fingerprint(ch, settings))
	if err != nil {
		t.Fatal(err)
	}
	return playback.Session
}
