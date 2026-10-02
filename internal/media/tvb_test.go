package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iptv-manager/internal/core"
)

type tvbTransport func(*http.Request) (*http.Response, error)

func (f tvbTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type tvbFixture struct {
	t         *testing.T
	mu        sync.Mutex
	api       map[string]int
	media     map[string]int
	clock     atomic.Int64
	deny      atomic.Bool
	denyFirst atomic.Bool
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
			http.SetCookie(w, &http.Cookie{Name: "hdntl", Value: want, Path: "/", Secure: true, MaxAge: 3600})
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
	repo := &integrationRepo{settings: core.Settings{DefaultMode: "relay", PlaybackToken: "tvb-test"}, traffic: map[string]int64{}}
	for _, code := range []string{"C", "F"} {
		repo.channels = append(repo.channels, core.Channel{ID: code, SourceType: "tvb", URL: "https://news.tvb.com/tc/live/" + code, Enabled: true, Mode: "inherit"})
	}
	srv := New(repo, &integrationResolver{}, Options{CacheBytes: 1 << 20, TVBClientFactory: func(string) (*http.Client, error) { return &http.Client{Transport: tvbTransport(f.request)}, nil }})
	srv.tvb.now = f.now
	local := httptest.NewServer(srv.Handler())
	repo.settings.BaseURL = local.URL
	t.Cleanup(func() { local.Close(); srv.tvb.invalidate("C"); srv.tvb.invalidate("F") })
	return srv, local, repo, f
}

func tvbWatch(local *httptest.Server, code string) string {
	return local.URL + "/watch/" + code + "?token=tvb-test"
}

func TestTVBRelayCookieIsolationCachingAndDirect(t *testing.T) {
	_, local, _, f := setupTVB(t)
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
	resp, _ := integrationGet(t, &client, tvbWatch(local, "F")+"&mode=direct", nil)
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
	srv.tvb.mu.Lock()
	old := srv.tvb.sessions["C"]
	srv.tvb.mu.Unlock()
	u, _ := url.Parse(old.root)
	if len(old.jar.Cookies(u)) == 0 {
		t.Fatal("session Cookie not retained")
	}
	srv.Invalidate("C")
	if len(old.jar.Cookies(u)) != 0 || old.ctx.Err() == nil {
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
			srv, local, _, f := setupTVB(t)
			_, master := integrationGet(t, local.Client(), tvbWatch(local, "C"), nil)
			if cookieExpiry {
				srv.tvb.mu.Lock()
				e := srv.tvb.sessions["C"]
				srv.tvb.mu.Unlock()
				u, _ := url.Parse(e.root)
				e.jar.SetCookies(u, []*http.Cookie{{Name: "hdntl", Value: "", Path: "/", MaxAge: -1}})
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

func TestTVBRejectsUnsafeAPIResultsAndCachesFailures(t *testing.T) {
	for name, data := range map[string]map[string]any{
		"foreign":       {"channel_id": "C", "protocol": "hls", "stream_url": "https://evil.invalid/signed?secret=value"},
		"local":         {"channel_id": "C", "protocol": "hls", "stream_url": "http://127.0.0.1/master.m3u8"},
		"wrong-channel": {"channel_id": "F", "protocol": "hls", "stream_url": "https://cdn.tvb.com/master.m3u8"},
		"geo":           {"channel_id": "C", "protocol": "hls", "stream_url": "https://cdn.tvb.com/master.m3u8", "geo_blocked": true},
		"dash":          {"channel_id": "C", "protocol": "dash", "stream_url": "https://cdn.tvb.com/master.mpd"},
		"expired":       {"channel_id": "C", "protocol": "hls", "stream_url": "https://cdn.tvb.com/master.m3u8", "expire_time": 1},
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			m := newTVBResolver(func(string) (*http.Client, error) {
				return &http.Client{Transport: tvbTransport(func(r *http.Request) (*http.Response, error) {
					calls.Add(1)
					w := httptest.NewRecorder()
					json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
					resp := w.Result()
					resp.Request = r
					return resp, nil
				})}, nil
			})
			ch := core.Channel{ID: "C", SourceType: "tvb", URL: "https://news.tvb.com/tc/live/C"}
			for i := 0; i < 2; i++ {
				_, err := m.resolve(context.Background(), ch, core.Settings{})
				if err == nil || strings.Contains(err.Error(), "secret") {
					t.Fatalf("unsafe response: %v", err)
				}
			}
			if calls.Load() != 1 {
				t.Fatal("failure not negatively cached")
			}
			m.invalidate("C")
		})
	}
}

func TestTVBRefreshDuringPendingAPIRequestCannotRestoreSession(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	m := newTVBResolver(func(string) (*http.Client, error) {
		return &http.Client{Transport: tvbTransport(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			close(started)
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}, nil
	})
	ch := core.Channel{ID: "C", SourceType: "tvb", URL: "https://news.tvb.com/tc/live/C"}
	done := make(chan error, 1)
	go func() { _, err := m.resolve(context.Background(), ch, core.Settings{}); done <- err }()
	<-started
	m.invalidate("C")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cleared flight restored a result")
		}
	case <-time.After(time.Second):
		t.Fatal("clear did not cancel pending request")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) != 0 || calls.Load() != 1 {
		t.Fatal("session restored after invalidation")
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

func TestTVBCookieScopeAndLateResponseAfterClear(t *testing.T) {
	jar := newTVBCookieJar()
	u, _ := url.Parse("https://cdn.tvb.com/C/master.m3u8")
	jar.SetCookies(u, []*http.Cookie{
		{Name: "supercookie", Value: "bad", Domain: ".com", Path: "/"},
		{Name: "foreign", Value: "bad", Domain: "news.tvb.com", Path: "/"},
		{Name: "hdntl", Value: "auth", Path: "/C/", Secure: true},
	})
	if cookies := jar.Cookies(u); len(cookies) != 1 || cookies[0].Name != "hdntl" || jar.expired(u.String()) {
		t.Fatal("Cookie scope not enforced")
	}
	other, _ := url.Parse("https://cdn.tvb.com/F/master.m3u8")
	if len(jar.Cookies(other)) != 0 {
		t.Fatal("Cookie leaked to a different path")
	}
	jar.clear()
	jar.SetCookies(u, []*http.Cookie{{Name: "hdntl", Value: "late", Path: "/"}})
	if len(jar.Cookies(u)) != 0 || !jar.expired(u.String()) {
		t.Fatal("late response restored cleared Cookie")
	}
}

func TestTVBProxyChangeReplacesSession(t *testing.T) {
	srv, local, repo, f := setupTVB(t)
	var proxies []string
	srv.tvb.client = func(proxy string) (*http.Client, error) {
		proxies = append(proxies, proxy)
		return &http.Client{Transport: tvbTransport(f.request)}, nil
	}
	_, master := integrationGet(t, local.Client(), tvbWatch(local, "C"), nil)
	old := srv.tvb.sessions["C"]
	repo.settings.UpstreamProxy = "socks5://proxy.example:1080"
	resp, _ := integrationGet(t, local.Client(), tvbWatch(local, "C"), nil)
	if resp.StatusCode != 200 || f.calls("C") != 2 || len(proxies) != 2 || proxies[1] != repo.settings.UpstreamProxy || old.ctx.Err() == nil {
		t.Fatal("proxy change reused the old session")
	}
	resp, _ = integrationGet(t, local.Client(), integrationLinks(master)[0], nil)
	if resp.StatusCode != 410 {
		t.Fatal("old proxy reference still accepted")
	}
}

func TestTVBTLSRetryIsBoundedAndLogsAreSanitized(t *testing.T) {
	var logs bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(original)
	handshake := &net.OpError{Op: "remote error", Err: errors.New("tls: handshake failure")}
	for _, scenario := range []struct {
		name                string
		failures, wantCalls int
		failure             error
	}{
		{"recover", 1, 2, handshake},
		{"bounded", 9, 3, handshake},
		{"no-other-retry", 1, 1, errors.New("certificate or network failure")},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: tvbTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls <= scenario.failures {
					return nil, scenario.failure
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("media")), Request: r}, nil
			})}
			req, _ := http.NewRequest("GET", "https://cdn.tvb.com/key?hdnea=private-token", nil)
			req.Header.Set("Cookie", "hdntl=private-cookie")
			resp, err := tvbMediaRequest(client, req, "C")
			if calls != scenario.wantCalls || (err == nil) != (scenario.name == "recover") {
				t.Fatalf("retry result: calls=%d error=%v", calls, err)
			}
			if resp != nil {
				resp.Body.Close()
			}
		})
	}
	if strings.Contains(logs.String(), "private-") || strings.Contains(logs.String(), "hdnea") || strings.Contains(logs.String(), "hdntl") {
		t.Fatal("signed URL or Cookie exposed in logs")
	}
}
