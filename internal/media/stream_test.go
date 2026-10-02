package media

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGenericStreamDirectAndHLSRelaySkipResolver(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/entry":
			http.Redirect(w, r, "/live/master", 302)
		case "/live/master":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",NAME=\"Main\",URI=\"audio\"\n#EXT-X-STREAM-INF:BANDWIDTH=1000,RESOLUTION=1280x720,AUDIO=\"audio\"\nvideo\n")
		case "/live/audio", "/live/video":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-KEY:METHOD=AES-128,URI=\"key\"\n#EXT-X-MAP:URI=\"init\"\n#EXTINF:6,\nsegment\n")
		default:
			w.Header().Set("Content-Type", "application/octet-stream")
			if r.Header.Get("Range") == "bytes=0-3" {
				w.Header().Set("Content-Range", "bytes 0-3/7")
				w.WriteHeader(206)
				fmt.Fprint(w, "payl")
			} else {
				fmt.Fprint(w, "payload")
			}
		}
	}))
	defer upstream.Close()
	srv, local, repo, res := integrationSetup(t, upstream)
	srv.options.StreamClientFactory = srv.options.ClientFactory
	srv.options.ValidateStreamURL = srv.options.ValidateURL
	repo.channels[0].SourceType = "stream"
	repo.channels[0].URL = upstream.URL + "/entry"
	c := *local.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, _ := integrationGet(t, &c, integrationWatch(local, "direct"), nil)
	if resp.StatusCode != 307 || resp.Header.Get("Location") != repo.channels[0].URL || calls.Load() != 0 || res.calls != 0 {
		t.Fatal("direct source was fetched or resolved")
	}
	resp, master := integrationGet(t, local.Client(), integrationWatch(local, "relay"), nil)
	links := integrationLinks(master)
	if resp.StatusCode != 200 || len(links) != 2 || strings.Contains(master, upstream.URL) {
		t.Fatalf("master rewrite: %d %s", resp.StatusCode, master)
	}
	for _, link := range links {
		_, media := integrationGet(t, local.Client(), link, nil)
		assets := integrationLinks(media)
		if len(assets) != 3 {
			t.Fatalf("missing key/map/segment: %s", media)
		}
		for _, asset := range assets {
			resp, body := integrationGet(t, local.Client(), asset, map[string]string{"Range": "bytes=0-3"})
			if resp.StatusCode != 206 || body != "payl" {
				t.Fatalf("range relay: %d %s", resp.StatusCode, body)
			}
		}
	}
	if res.calls != 0 || res.invalidations != 0 {
		t.Fatal("generic source invoked YouTube resolver")
	}
	if srv.Statuses()[repo.channels[0].ID].State != "ready" {
		t.Fatal("successful generic segment erased ready status")
	}
	repo.channels[0].SourceMissing = true
	resp, _ = integrationGet(t, local.Client(), links[0], nil)
	if resp.StatusCode != 410 {
		t.Fatalf("missing source remained playable: %d", resp.StatusCode)
	}
	resp, _ = integrationGet(t, local.Client(), integrationWatch(local, "direct"), nil)
	if resp.StatusCode != 404 {
		t.Fatal("missing direct source remained playable")
	}
}

func TestContinuousHTTPStreamHasIndependentViewers(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		fmt.Fprint(w, strings.Repeat("T", 512))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	srv, local, repo, res := integrationSetup(t, upstream)
	srv.options.StreamClientFactory = func(string) (*http.Client, error) { return &http.Client{Timeout: time.Nanosecond}, nil }
	srv.options.ValidateStreamURL = srv.options.ValidateURL
	repo.channels[0].SourceType = "stream"
	repo.channels[0].URL = upstream.URL
	client := &http.Client{Timeout: 2 * time.Second}
	for range 2 {
		resp, err := client.Get(integrationWatch(local, "relay"))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		buf := make([]byte, 256)
		if _, err := io.ReadFull(resp.Body, buf); err != nil || string(buf) != strings.Repeat("T", 256) {
			t.Fatalf("live stream buffered or timed out: %v", err)
		}
	}
	if res.calls != 0 {
		t.Fatal("continuous stream invoked resolver")
	}
}

func TestGenericRelayBlocksUnsafeResources(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nhttp://169.254.169.254/latest\n")
	}))
	defer upstream.Close()
	srv, local, repo, res := integrationSetup(t, upstream)
	srv.options.StreamClientFactory = srv.options.ClientFactory
	srv.options.ValidateStreamURL = srv.options.ValidateURL
	repo.channels[0].SourceType = "stream"
	repo.channels[0].URL = upstream.URL
	resp, _ := integrationGet(t, local.Client(), integrationWatch(local, "relay"), nil)
	if resp.StatusCode != 502 || res.calls != 0 {
		t.Fatalf("unsafe resource accepted: %d", resp.StatusCode)
	}
}
