package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"iptv-manager/internal/core"
	"iptv-manager/internal/resolver"
)

type integrationRepo struct {
	mu          sync.Mutex
	channels    []core.Channel
	settings    core.Settings
	traffic     map[string]int64
	failTraffic bool
}

func (r *integrationRepo) Channels(context.Context) ([]core.Channel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.Channel(nil), r.channels...), nil
}
func (r *integrationRepo) Channel(_ context.Context, id string) (core.Channel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ch := range r.channels {
		if ch.ID == id {
			return ch, nil
		}
	}
	return core.Channel{}, errors.New("missing channel")
}
func (r *integrationRepo) Settings(context.Context) (core.Settings, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settings, nil
}
func (r *integrationRepo) AddTraffic(_ context.Context, month string, bytes int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failTraffic {
		return errors.New("temporary storage failure")
	}
	r.traffic[month] += bytes
	return nil
}

type integrationResolver struct {
	mu            sync.Mutex
	results       []resolver.Result
	calls         int
	invalidations int
}

func (r *integrationResolver) Resolve(context.Context, core.Channel, core.Settings) (resolver.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	index := r.calls
	r.calls++
	if index >= len(r.results) {
		index = len(r.results) - 1
	}
	return r.results[index], nil
}
func (r *integrationResolver) Invalidate(string) { r.mu.Lock(); r.invalidations++; r.mu.Unlock() }

func integrationSetup(t *testing.T, upstream *httptest.Server, results ...string) (*Server, *httptest.Server, *integrationRepo, *integrationResolver) {
	t.Helper()
	repo := &integrationRepo{
		settings: core.Settings{DefaultMode: "relay", DefaultQuality: 720, UpstreamProxy: "direct", PlaybackToken: "test-playback-token", MonthlyBudgetGB: 800},
		channels: []core.Channel{{ID: "stable-one", Name: "新闻频道", URL: "https://www.youtube.com/watch?v=vr3XyVCR4T0", Enabled: true, Mode: "inherit", Proxy: "inherit", SortOrder: 0}, {ID: "disabled", Name: "停用频道", Enabled: false, SortOrder: 1}},
		traffic:  make(map[string]int64),
	}
	if len(results) == 0 {
		results = []string{upstream.URL + "/master.m3u8"}
	}
	res := &integrationResolver{}
	for _, raw := range results {
		res.results = append(res.results, resolver.Result{URL: raw, Height: 720, ExpiresAt: time.Now().Add(time.Hour), Headers: map[string]string{"User-Agent": "Integration/1.0"}})
	}
	srv := New(repo, res, Options{CacheBytes: 1 << 20, ClientFactory: func(string) (*http.Client, error) { return upstream.Client(), nil }, ValidateURL: func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		base, _ := url.Parse(upstream.URL)
		if u.Scheme != base.Scheme || u.Host != base.Host {
			return errors.New("unexpected source")
		}
		return nil
	}})
	local := httptest.NewServer(srv.Handler())
	repo.settings.BaseURL = local.URL
	t.Cleanup(local.Close)
	return srv, local, repo, res
}

func integrationGet(t *testing.T, client *http.Client, raw string, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest("GET", raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

func integrationWatch(local *httptest.Server, mode string) string {
	return local.URL + "/watch/stable-one?token=test-playback-token&mode=" + mode
}

var integrationURI = regexp.MustCompile(`URI="([^"]+)"`)

func integrationLinks(body string) []string {
	var links []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			links = append(links, line)
			continue
		}
		for _, match := range integrationURI.FindAllStringSubmatch(line, -1) {
			links = append(links, match[1])
		}
	}
	return links
}

func TestIntegrationPlaylistModesKeepStableIDsAndExcludeDisabled(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()
	_, local, repo, _ := integrationSetup(t, upstream)
	for _, mode := range []string{"default", "relay", "direct"} {
		resp, body := integrationGet(t, local.Client(), local.URL+"/playlist.m3u?mode="+mode+"&token=test-playback-token", nil)
		if resp.StatusCode != 200 || !strings.HasPrefix(body, "#EXTM3U\n") {
			t.Fatalf("playlist %s: %d %s", mode, resp.StatusCode, body)
		}
		if strings.Contains(body, "disabled") || strings.Contains(body, "停用频道") {
			t.Fatal("disabled channel in playlist")
		}
		if !strings.Contains(body, `tvg-id="stable-one"`) {
			t.Fatal("stable channel ID missing")
		}
		links := integrationLinks(body)
		if len(links) != 1 {
			t.Fatalf("expected one playback link: %v", links)
		}
		u, _ := url.Parse(links[0])
		if u.Path != "/watch/stable-one" || u.Query().Get("mode") != mode {
			t.Fatalf("unexpected link: %s", links[0])
		}
	}
	repo.mu.Lock()
	repo.channels[0].Name = "重命名"
	repo.channels[0].SortOrder = 99
	repo.mu.Unlock()
	_, body := integrationGet(t, local.Client(), local.URL+"/playlist.m3u?token=test-playback-token", nil)
	if !strings.Contains(body, "/watch/stable-one?") || !strings.Contains(body, "重命名") {
		t.Fatal("rename or reorder changed playback identity")
	}
	for _, path := range []string{"/playlist.m3u", "/watch/stable-one", "/media/unknown"} {
		resp, _ := integrationGet(t, local.Client(), local.URL+path, nil)
		if resp.StatusCode != 401 {
			t.Errorf("unauthenticated %s: %d", path, resp.StatusCode)
		}
	}
}

func TestIntegrationDirectRedirectDoesNotFetchMedia(t *testing.T) {
	var fetched atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fetched.Add(1); w.WriteHeader(500) }))
	defer upstream.Close()
	_, local, repo, res := integrationSetup(t, upstream)
	client := *local.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, _ := integrationGet(t, &client, integrationWatch(local, "direct"), nil)
	if resp.StatusCode != http.StatusTemporaryRedirect || resp.Header.Get("Location") != upstream.URL+"/master.m3u8" {
		t.Fatalf("direct redirect: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if fetched.Load() != 0 || res.calls != 1 {
		t.Fatal("direct mode downloaded media or did not resolve")
	}
	repo.mu.Lock()
	repo.settings.DefaultMode = "direct"
	repo.mu.Unlock()
	resp, _ = integrationGet(t, &client, integrationWatch(local, "default"), nil)
	if resp.StatusCode != 307 || fetched.Load() != 0 {
		t.Fatal("default mode did not inherit direct setting")
	}
}

func TestIntegrationHLSRewritesAllNestedResources(t *testing.T) {
	var mu sync.Mutex
	seen := make(map[string]int)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path]++
		mu.Unlock()
		if r.Header.Get("User-Agent") != "Integration/1.0" {
			t.Error("source request headers not propagated")
		}
		switch r.URL.Path {
		case "/master.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			io.WriteString(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"Main\",URI=\"audio/list.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=3000000,AUDIO=\"audio\"\nvideo/list.m3u8\n")
		case "/audio/list.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXTINF:4,\n../audio.aac\n")
		case "/video/list.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"../key.bin\"\n#EXT-X-MAP:URI=\"../init.mp4\"\n#EXTINF:4,\n../video.ts?part=1\n")
		case "/audio.aac", "/key.bin", "/init.mp4", "/video.ts":
			w.Header().Set("Content-Type", "application/octet-stream")
			io.WriteString(w, "media "+r.URL.Path)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	_, local, _, _ := integrationSetup(t, upstream)
	queue := []string{integrationWatch(local, "relay")}
	visited := make(map[string]bool)
	for len(queue) > 0 {
		raw := queue[0]
		queue = queue[1:]
		if visited[raw] {
			continue
		}
		visited[raw] = true
		resp, body := integrationGet(t, local.Client(), raw, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("nested resource: %d %s", resp.StatusCode, body)
		}
		if strings.HasPrefix(body, "#EXTM3U") {
			if strings.Contains(body, upstream.URL) {
				t.Fatal("manifest leaked an upstream media URL")
			}
			for _, link := range integrationLinks(body) {
				u, err := url.Parse(link)
				if err != nil || !strings.HasPrefix(link, local.URL+"/media/") || u.Query().Get("token") != "test-playback-token" {
					t.Fatalf("resource not relayed: %s", link)
				}
				queue = append(queue, link)
			}
		}
	}
	for _, path := range []string{"/master.m3u8", "/audio/list.m3u8", "/video/list.m3u8", "/audio.aac", "/key.bin", "/init.mp4", "/video.ts"} {
		if seen[path] != 1 {
			t.Errorf("%s fetched %d times", path, seen[path])
		}
	}
}

func TestIntegrationRangeCacheAndTrafficAccounting(t *testing.T) {
	var segments atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/master.m3u8" {
			io.WriteString(w, "#EXTM3U\n#EXTINF:4,\nsegment.ts\n")
			return
		}
		segments.Add(1)
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Header.Get("Range") != "" {
			if r.Header.Get("Range") != "bytes=2-5" {
				t.Errorf("incorrect Range: %s", r.Header.Get("Range"))
			}
			w.Header().Set("Content-Range", "bytes 2-5/10")
			w.Header().Set("Content-Length", "4")
			w.WriteHeader(http.StatusPartialContent)
			io.WriteString(w, "2345")
			return
		}
		w.Header().Set("Content-Length", "10")
		io.WriteString(w, "0123456789")
	}))
	defer upstream.Close()
	srv, local, repo, _ := integrationSetup(t, upstream)
	_, manifest := integrationGet(t, local.Client(), integrationWatch(local, "relay"), nil)
	link := integrationLinks(manifest)[0]
	for i := 0; i < 2; i++ {
		resp, body := integrationGet(t, local.Client(), link, map[string]string{"Range": "bytes=2-5"})
		if resp.StatusCode != 206 || resp.Header.Get("Content-Range") != "bytes 2-5/10" || body != "2345" {
			t.Fatalf("bad range: %d %v %s", resp.StatusCode, resp.Header, body)
		}
	}
	for i := 0; i < 2; i++ {
		resp, body := integrationGet(t, local.Client(), link, nil)
		if resp.StatusCode != 200 || body != "0123456789" {
			t.Fatal("full body corrupted by range cache")
		}
	}
	if segments.Load() != 2 {
		t.Fatalf("cache did not distinguish/reuse full and ranged requests: %d fetches", segments.Load())
	}
	repo.mu.Lock()
	repo.failTraffic = true
	repo.mu.Unlock()
	if err := srv.FlushTraffic(context.Background()); err == nil {
		t.Fatal("flush should report storage failure")
	}
	repo.mu.Lock()
	repo.failTraffic = false
	repo.mu.Unlock()
	if err := srv.FlushTraffic(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := srv.FlushTraffic(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := int64(len(manifest) + 2*4 + 2*10)
	repo.mu.Lock()
	got := repo.traffic[time.Now().UTC().Format("2006-01")]
	repo.mu.Unlock()
	if got != want {
		t.Fatalf("traffic (including cache hits) got %d want %d", got, want)
	}
}

func TestIntegrationConcurrentSegmentRequestsShareOneFetch(t *testing.T) {
	var count atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/master.m3u8" {
			io.WriteString(w, "#EXTM3U\n#EXTINF:4,\nsegment.ts\n")
			return
		}
		count.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("Content-Length", "7")
		io.WriteString(w, "segment")
	}))
	defer upstream.Close()
	_, local, _, _ := integrationSetup(t, upstream)
	_, manifest := integrationGet(t, local.Client(), integrationWatch(local, "relay"), nil)
	link := integrationLinks(manifest)[0]
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := local.Client().Get(link)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != 200 || string(body) != "segment" {
				t.Errorf("concurrent response %d: %s %v", resp.StatusCode, body, err)
			}
		}()
	}
	close(start)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("source request never started")
	}
	close(release)
	wg.Wait()
	if count.Load() != 1 {
		t.Fatalf("same segment fetched %d times", count.Load())
	}
}

func TestIntegrationExpiredSourceReResolvesAndRevokesOldAccess(t *testing.T) {
	var expired atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/expired.m3u8":
			expired.Add(1)
			w.WriteHeader(http.StatusForbidden)
		case "/fresh.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXTINF:4,\nsegment.ts\n")
		case "/segment.ts":
			io.WriteString(w, "content")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	_, local, repo, res := integrationSetup(t, upstream, upstream.URL+"/expired.m3u8", upstream.URL+"/fresh.m3u8")
	resp, manifest := integrationGet(t, local.Client(), integrationWatch(local, "relay"), nil)
	// A newer source is already available from Resolve; do not invalidate it
	// and cancel another viewer's refresh unnecessarily.
	if resp.StatusCode != 200 || expired.Load() != 1 || res.calls != 2 || res.invalidations != 0 {
		t.Fatalf("source recovery: %d calls=%d invalidations=%d", resp.StatusCode, res.calls, res.invalidations)
	}
	link := integrationLinks(manifest)[0]
	resp, _ = integrationGet(t, local.Client(), link, nil)
	if resp.StatusCode != 200 {
		t.Fatal("fresh segment unavailable")
	}
	repo.mu.Lock()
	repo.settings.PlaybackToken = "rotated-token"
	repo.mu.Unlock()
	resp, _ = integrationGet(t, local.Client(), link, nil)
	if resp.StatusCode != 401 {
		t.Fatalf("old token survived rotation: %d", resp.StatusCode)
	}
	u, _ := url.Parse(link)
	q := u.Query()
	q.Set("token", "rotated-token")
	u.RawQuery = q.Encode()
	repo.mu.Lock()
	repo.channels[0].Enabled = false
	repo.mu.Unlock()
	resp, _ = integrationGet(t, local.Client(), u.String(), nil)
	if resp.StatusCode != 410 {
		t.Fatalf("disabled channel served cached media: %d", resp.StatusCode)
	}
	resp, _ = integrationGet(t, local.Client(), fmt.Sprintf("%s/watch/stable-one?token=rotated-token", local.URL), nil)
	if resp.StatusCode != 404 {
		t.Fatalf("disabled watch: %d", resp.StatusCode)
	}
}
