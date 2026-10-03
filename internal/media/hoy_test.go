package media

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
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
	"iptv-manager/internal/provider"
)

type hoyFixture struct {
	t         *testing.T
	mu        sync.Mutex
	api       map[string]int
	media     map[string]int
	clock     atomic.Int64
	deny      atomic.Bool
	denyFirst atomic.Bool
}

func (f *hoyFixture) now() time.Time        { return time.Unix(f.clock.Load(), 0) }
func (f *hoyFixture) calls(code string) int { f.mu.Lock(); defer f.mu.Unlock(); return f.api[code] }

func (f *hoyFixture) request(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
		f.t.Error("HOY request replayed credentials")
	}
	if r.URL.Hostname() == "api2.hoy.tv" {
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/v3/a/liveCheckout/"))
		if id < 1 || id > 3 || r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
			f.t.Error("wrong HOY checkout request")
		}
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			if len(b) != 0 {
				f.t.Error("unexpected checkout body")
			}
		}
		code := strconv.Itoa(id + 75)
		f.api[code]++
		root := "https://ch" + code + "-live-stream.hoy.tv/ch" + code + "/"
		policy := fmt.Sprintf(`{"Statement":[{"Resource":%q,"Condition":{"DateLessThan":{"AWS:EpochTime":%d}}}]}`, root+"*", f.now().Add(90*time.Second).Unix())
		encoded := strings.NewReplacer("+", "-", "=", "_", "/", "~").Replace(base64.StdEncoding.EncodeToString([]byte(policy)))
		// All channels use the same .hoy.tv cookie names. URL signing must keep
		// them isolated without sending these response cookies to any client.
		http.SetCookie(w, &http.Cookie{Name: "CloudFront-Signature", Value: "do-not-replay", Domain: ".hoy.tv", Path: "/"})
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": map[string]any{"id": id, "video": map[string]any{"id": id + 75, "link": root + "index-fhd.m3u8"}, "signed": map[string]string{"CloudFront-Policy": encoded, "CloudFront-Key-Pair-Id": "test-key", "CloudFront-Signature": fmt.Sprintf("private-%s-%d", code, f.api[code])}}})
	} else {
		code := strings.TrimSuffix(strings.TrimPrefix(r.URL.Hostname(), "ch"), "-live-stream.hoy.tv")
		file := strings.TrimPrefix(r.URL.Path, "/ch"+code+"/")
		q := r.URL.Query()
		want := fmt.Sprintf("private-%s-%d", code, f.api[code])
		if q.Get("Signature") != want || len(q["Signature"]) != 1 || q.Get("Policy") == "" || q.Get("Key-Pair-Id") != "test-key" {
			f.t.Error("missing, stale or mixed HOY signatures")
			w.WriteHeader(403)
		} else if f.deny.Load() || (f.denyFirst.Load() && f.api[code] == 1) {
			w.WriteHeader(403)
		} else if file == "index-fhd.m3u8" {
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"main\",NAME=\"Chinese\",DEFAULT=YES,URI=\"audio.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720,CODECS=\"avc1,mp4a\",AUDIO=\"main\"\nvideo.m3u8?Signature=stale&keep=yes\n")
		} else if strings.HasSuffix(file, ".m3u8") {
			if file == "video.m3u8" && q.Get("keep") != "yes" {
				f.t.Error("playlist query lost")
			}
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6,\n%s.ts\n", strings.TrimSuffix(file, ".m3u8"))
		} else {
			f.media[code+"/"+file]++
			if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
				if rangeHeader != "bytes=0-3" {
					f.t.Error("range lost")
				}
				w.Header().Set("Content-Range", "bytes 0-3/100")
				w.WriteHeader(206)
				fmt.Fprint(w, "part")
			} else {
				fmt.Fprintf(w, "payload %s/%d/%s", code, f.api[code], file)
			}
		}
		if r.Header.Get("Referer") != "https://hoy.tv/live?channel_no="+code || r.Header.Get("Origin") != "https://hoy.tv" || r.Header.Get("User-Agent") == "" {
			f.t.Error("HOY request headers lost")
		}
	}
	resp := w.Result()
	resp.Request = r
	return resp, nil
}

func setupHOY(t *testing.T) (*Server, *httptest.Server, *integrationRepo, *hoyFixture) {
	t.Helper()
	f := &hoyFixture{t: t, api: map[string]int{}, media: map[string]int{}}
	f.clock.Store(time.Now().Unix())
	repo := &integrationRepo{settings: core.Settings{DefaultMode: "relay", PlaybackToken: "hoy-test"}, traffic: map[string]int64{}}
	for _, code := range []string{"76", "77", "78"} {
		repo.channels = append(repo.channels, core.Channel{ID: code, SourceType: "builtin", ProviderID: "hoy", ProviderChannelID: code, URL: "https://hoy.tv/live?channel_no=" + code, Enabled: true, Mode: "inherit"})
	}
	srv := New(repo, &integrationResolver{}, Options{CacheBytes: 1 << 20, ProviderOptions: provider.Options{Now: f.now, ClientFactory: func(string) (*http.Client, error) { return &http.Client{Transport: tvbTransport(f.request)}, nil }}})
	local := httptest.NewServer(srv.Handler())
	repo.settings.BaseURL = local.URL
	t.Cleanup(func() {
		local.Close()
		for _, code := range []string{"76", "77", "78"} {
			srv.providers.Invalidate(code)
		}
	})
	return srv, local, repo, f
}

func hoyWatch(local *httptest.Server, code string) string {
	return local.URL + "/watch/" + code + "?token=hoy-test"
}

func TestHOYRelaySignsAllResourcesAndIsolatesChannels(t *testing.T) {
	_, local, _, f := setupHOY(t)
	for _, code := range []string{"76", "77", "78", "76"} {
		resp, master := integrationGet(t, local.Client(), hoyWatch(local, code), nil)
		if resp.StatusCode != 200 || strings.Contains(master, "Signature") || strings.Contains(master, "hoy.tv") || resp.Header.Get("Set-Cookie") != "" {
			t.Fatalf("unsafe master: %d %s", resp.StatusCode, master)
		}
		for _, link := range integrationLinks(master) {
			resp, playlist := integrationGet(t, local.Client(), link, nil)
			if resp.StatusCode != 200 {
				t.Fatalf("playlist failed: %d %s", resp.StatusCode, playlist)
			}
			for _, part := range integrationLinks(playlist) {
				resp, body := integrationGet(t, local.Client(), part, nil)
				if resp.StatusCode != 200 || !strings.HasPrefix(body, "payload "+code+"/1/") {
					t.Fatalf("resource failed: %d %s", resp.StatusCode, body)
				}
				resp, _ = integrationGet(t, local.Client(), part, nil)
				if resp.StatusCode != 200 {
					t.Fatal("cached resource failed")
				}
			}
		}
	}
	for _, code := range []string{"76", "77", "78"} {
		if f.calls(code) != 1 {
			t.Fatal("playback reran checkout")
		}
	}
	f.mu.Lock()
	for _, count := range f.media {
		if count != 1 {
			t.Error("resource cache not reused")
		}
	}
	f.mu.Unlock()
	client := *local.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, _ := integrationGet(t, &client, hoyWatch(local, "78")+"&mode=direct", nil)
	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || resp.StatusCode != 307 || u.Hostname() != "ch78-live-stream.hoy.tv" || u.Query().Get("Signature") != "private-78-1" || resp.Header.Get("Set-Cookie") != "" {
		t.Fatal("direct URL not signed")
	}
}

func TestHOYExpiryAnd403RenewBothTracks(t *testing.T) {
	for _, scenario := range []string{"expiry", "403"} {
		t.Run(scenario, func(t *testing.T) {
			_, local, _, f := setupHOY(t)
			_, master := integrationGet(t, local.Client(), hoyWatch(local, "76"), nil)
			_, oldPlaylist := integrationGet(t, local.Client(), integrationLinks(master)[0], nil)
			oldPart := integrationLinks(oldPlaylist)[2]
			if scenario == "expiry" {
				f.clock.Add(61)
			} else {
				f.denyFirst.Store(true)
			}
			if f.calls("76") != 1 {
				t.Fatal("background refresh occurred")
			}
			for _, track := range integrationLinks(master) {
				resp, playlist := integrationGet(t, local.Client(), track, nil)
				if resp.StatusCode != 200 {
					t.Fatalf("track refresh failed: %d %s", resp.StatusCode, playlist)
				}
				resp, body := integrationGet(t, local.Client(), integrationLinks(playlist)[2], map[string]string{"Range": "bytes=0-3"})
				if resp.StatusCode != 206 || body != "part" || resp.Header.Get("Content-Range") != "bytes 0-3/100" {
					t.Fatal("refreshed range failed")
				}
			}
			if f.calls("76") != 2 {
				t.Fatal("tracks did not share renewal")
			}
			resp, _ := integrationGet(t, local.Client(), oldPart, nil)
			if resp.StatusCode != 410 {
				t.Fatal("old segment silently substituted")
			}
		})
	}
}

func TestHOYManualRefreshAndProxyChangeInvalidateOldResources(t *testing.T) {
	for _, scenario := range []string{"manual", "proxy"} {
		t.Run(scenario, func(t *testing.T) {
			srv, local, repo, f := setupHOY(t)
			var proxies []string
			srv.providers = provider.New(provider.Options{Now: f.now, ClientFactory: func(proxy string) (*http.Client, error) {
				proxies = append(proxies, proxy)
				return &http.Client{Transport: tvbTransport(f.request)}, nil
			}})
			_, master := integrationGet(t, local.Client(), hoyWatch(local, "77"), nil)
			track := integrationLinks(master)[0]
			_, playlist := integrationGet(t, local.Client(), track, nil)
			part := integrationLinks(playlist)[2]
			_, _ = integrationGet(t, local.Client(), part, nil)
			if scenario == "manual" {
				srv.Invalidate("77")
				if srv.cache.used != 0 || f.calls("77") != 1 {
					t.Fatal("manual refresh retained cache or eagerly resolved")
				}
			} else {
				repo.settings.Proxies = []core.Proxy{{ID: "0123456789abcdef01234567", Name: "测试代理", Scheme: "socks5", Host: "proxy.example", Port: 1080}}
				repo.settings.ProviderProxies = map[string]string{"hoy": "0123456789abcdef01234567"}
			}
			resp, _ := integrationGet(t, local.Client(), hoyWatch(local, "77"), nil)
			if resp.StatusCode != 200 || f.calls("77") != 2 {
				t.Fatal("session not replaced")
			}
			if scenario == "proxy" && (len(proxies) != 2 || proxies[1] != "socks5://proxy.example:1080") {
				t.Fatal("new proxy not used")
			}
			for _, old := range []string{track, part} {
				resp, _ := integrationGet(t, local.Client(), old, nil)
				if resp.StatusCode != 410 {
					t.Fatal("old resource survived")
				}
			}
		})
	}
}

func TestHOYPersistent403HasBoundedRecovery(t *testing.T) {
	srv, local, _, f := setupHOY(t)
	f.deny.Store(true)
	resp, _ := integrationGet(t, local.Client(), hoyWatch(local, "78"), nil)
	if resp.StatusCode != 502 || f.calls("78") != 2 {
		t.Fatal("403 did not retry exactly once")
	}
	for i := 0; i < 3; i++ {
		resp, _ = integrationGet(t, local.Client(), hoyWatch(local, "78"), nil)
		if resp.StatusCode != 503 {
			t.Fatal("missing cooldown")
		}
	}
	if f.calls("78") != 2 {
		t.Fatal("403 retry storm")
	}
	f.deny.Store(false)
	srv.Invalidate("78")
	resp, _ = integrationGet(t, local.Client(), hoyWatch(local, "78"), nil)
	if resp.StatusCode != 200 || f.calls("78") != 3 {
		t.Fatal("manual refresh did not unblock")
	}
}

func TestHOYConcurrentViewersShareCheckout(t *testing.T) {
	_, local, _, f := setupHOY(t)
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			resp, err := local.Client().Get(hoyWatch(local, "76"))
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
	if f.calls("76") != 1 {
		t.Fatal("concurrent viewers repeated checkout")
	}
}
