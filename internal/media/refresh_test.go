package media

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"youtube-tv/internal/core"
	"youtube-tv/internal/resolver"
)

type refreshRepo struct {
	ch       core.Channel
	settings core.Settings
}

func (r *refreshRepo) Channels(context.Context) ([]core.Channel, error) {
	return []core.Channel{r.ch}, nil
}
func (r *refreshRepo) Channel(context.Context, string) (core.Channel, error) { return r.ch, nil }
func (r *refreshRepo) Settings(context.Context) (core.Settings, error)       { return r.settings, nil }
func (r *refreshRepo) AddTraffic(context.Context, string, int64) error       { return nil }

type refreshResolver struct {
	mu            sync.Mutex
	current, next resolver.Result
	invalidations int
}

func (r *refreshResolver) Resolve(context.Context, core.Channel, core.Settings) (resolver.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current, nil
}
func (r *refreshResolver) Invalidate(string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current = r.next
	r.invalidations++
}

func refreshOptions() Options {
	return Options{MaxResources: 64, MaxFetches: 2, ValidateURL: func(string) error { return nil }, ClientFactory: func(string) (*http.Client, error) { return &http.Client{Timeout: time.Second}, nil }}
}

func getRefreshBody(t *testing.T, raw string) (int, string) {
	t.Helper()
	response, err := http.Get(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(body)
}

func firstRefreshURI(t *testing.T, body string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	t.Fatalf("missing resource URI in %q", body)
	return ""
}

func TestNestedPlaylistRefreshPreservesRenditionAfterReordering(t *testing.T) {
	var expired atomic.Bool
	var wrongRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		switch r.URL.Path {
		case "/old-master.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720,CODECS=\"avc1,mp4a\"\n/old-level.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=4000000,RESOLUTION=1920x1080\n/wrong.m3u8\n")
		case "/new-master.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=4000000,RESOLUTION=1920x1080\n/wrong.m3u8\n#EXT-X-STREAM-INF:CODECS=\"avc1,mp4a\",RESOLUTION=1280x720,BANDWIDTH=2000000\n/new-level.m3u8\n")
		case "/old-level.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720\n/old-final.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=854x480\n/wrong.m3u8\n")
		case "/new-level.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=854x480\n/wrong.m3u8\n#EXT-X-STREAM-INF:RESOLUTION=1280x720,BANDWIDTH=2000000\n/new-final.m3u8\n")
		case "/old-final.m3u8":
			if expired.Load() {
				http.Error(w, "expired", http.StatusForbidden)
				return
			}
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:100\n#EXTINF:4,\n/old-segment.ts\n")
		case "/new-final.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:120\n#EXTINF:4,\n/new-segment.ts\n")
		case "/new-segment.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			fmt.Fprint(w, "new-segment")
		case "/old-segment.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			fmt.Fprint(w, "old-segment")
		default:
			wrongRequests.Add(1)
			http.Error(w, "wrong rendition", http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	repo := &refreshRepo{ch: core.Channel{ID: "one", URL: "https://www.youtube.com/watch?v=vr3XyVCR4T0", Enabled: true, Mode: "relay", Quality: 720}, settings: core.Settings{PlaybackToken: "test-token", DefaultMode: "relay", DefaultQuality: 720}}
	res := &refreshResolver{current: resolver.Result{URL: upstream.URL + "/old-master.m3u8", ExpiresAt: time.Now().Add(time.Hour)}, next: resolver.Result{URL: upstream.URL + "/new-master.m3u8", ExpiresAt: time.Now().Add(time.Hour)}}
	s := New(repo, res, refreshOptions())
	gateway := httptest.NewServer(s.Handler())
	defer gateway.Close()
	repo.settings.BaseURL = gateway.URL
	status, body := getRefreshBody(t, gateway.URL+"/watch/one?token=test-token")
	if status != 200 {
		t.Fatalf("initial master %d: %s", status, body)
	}
	status, body = getRefreshBody(t, firstRefreshURI(t, body))
	if status != 200 {
		t.Fatalf("nested master %d: %s", status, body)
	}
	finalURI := firstRefreshURI(t, body)
	status, body = getRefreshBody(t, finalURI)
	if status != 200 || !strings.Contains(body, "SEQUENCE:100") {
		t.Fatalf("initial media %d: %s", status, body)
	}
	expired.Store(true)
	status, body = getRefreshBody(t, finalURI)
	if status != 200 || !strings.Contains(body, "SEQUENCE:120") {
		t.Fatalf("refreshed media %d: %s", status, body)
	}
	status, segment := getRefreshBody(t, firstRefreshURI(t, body))
	if status != 200 || segment != "new-segment" {
		t.Fatalf("refreshed segment %d: %s", status, segment)
	}
	status, body = getRefreshBody(t, finalURI)
	if status != 200 || !strings.Contains(body, "SEQUENCE:120") {
		t.Fatal("stable nested URI did not retain refreshed source")
	}
	res.mu.Lock()
	invalidations := res.invalidations
	res.mu.Unlock()
	if invalidations != 1 || wrongRequests.Load() != 0 {
		t.Fatalf("invalidations=%d wrong rendition requests=%d", invalidations, wrongRequests.Load())
	}
}

func TestSemanticLocatorMatchesAudioAndRejectsAmbiguity(t *testing.T) {
	base, _ := url.Parse("https://manifest.googlevideo.com/root.m3u8")
	selector := playlistSelector(`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="Chinese",LANGUAGE="zh",URI="old.m3u8"`)
	body := []byte("#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"English\",LANGUAGE=\"en\",URI=\"wrong.m3u8\"\n#EXT-X-MEDIA:URI=\"new.m3u8\",LANGUAGE=\"zh\",NAME=\"Chinese\",GROUP-ID=\"audio\",TYPE=AUDIO\n")
	found, err := findPlaylist(body, base, selector)
	if err != nil || found != "https://manifest.googlevideo.com/new.m3u8" {
		t.Fatalf("audio locator: %s %v", found, err)
	}
	ambiguous := append(append([]byte(nil), body...), []byte("#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"Chinese\",LANGUAGE=\"zh\",URI=\"another.m3u8\"\n")...)
	if _, err := findPlaylist(ambiguous, base, selector); err == nil {
		t.Fatal("ambiguous rendition accepted")
	}
	if _, err := findPlaylist([]byte("#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:120\n#EXTINF:4,\nnew.ts\n"), base, selector); err == nil {
		t.Fatal("media segment was used as a playlist locator")
	}
}

func TestActivePlaylistReferenceIsRenewedAndSurvivesSegmentEviction(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:4\n") }))
	defer upstream.Close()
	repo := &refreshRepo{ch: core.Channel{ID: "one", Enabled: true}, settings: core.Settings{PlaybackToken: "test-token"}}
	opts := refreshOptions()
	opts.MaxResources = 2
	s := New(repo, &refreshResolver{}, opts)
	gateway := httptest.NewServer(s.Handler())
	defer gateway.Close()
	repo.settings.BaseURL = gateway.URL
	ref := resource{URL: upstream.URL + "/live.m3u8", Channel: "one", Fingerprint: fingerprint(repo.ch, repo.settings), Playlist: true}
	uri, err := s.reference(ref, repo.settings)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(uri)
	id := strings.TrimPrefix(u.Path, "/media/")
	s.mu.Lock()
	stored := s.resources[id]
	stored.Expires = time.Now().Add(time.Second)
	s.resources[id] = stored
	s.mu.Unlock()
	if status, body := getRefreshBody(t, uri); status != 200 {
		t.Fatalf("active resource %d: %s", status, body)
	}
	s.mu.Lock()
	expiry := s.resources[id].Expires
	s.mu.Unlock()
	if time.Until(expiry) < 119*time.Minute {
		t.Fatal("active playlist TTL was not renewed")
	}
	for _, name := range []string{"first.ts", "second.ts", "third.ts"} {
		ref.URL = upstream.URL + "/" + name
		ref.Playlist = false
		if _, err := s.reference(ref, repo.settings); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	_, retained := s.resources[id]
	count := len(s.resources)
	s.mu.Unlock()
	if !retained || count != 2 {
		t.Fatalf("active playlist evicted: retained=%v resources=%d", retained, count)
	}
}

func TestMediaFlightQueueHasAnUpperBound(t *testing.T) {
	s := New(&refreshRepo{}, &refreshResolver{}, refreshOptions())
	for i := 0; i < 64; i++ {
		if wait, leader := s.begin(fmt.Sprint(i)); wait == nil || !leader {
			t.Fatalf("queue rejected slot %d", i)
		}
	}
	if wait, leader := s.begin("overflow"); wait != nil || leader {
		t.Fatal("queue exceeded cap")
	}
	if wait, leader := s.begin("0"); wait == nil || leader {
		t.Fatal("same-key viewer did not share existing flight")
	}
	s.end("0")
	if wait, leader := s.begin("after-release"); wait == nil || !leader {
		t.Fatal("completed slot was not reusable")
	}
}
