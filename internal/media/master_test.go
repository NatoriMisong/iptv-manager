package media

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"youtube-tv/internal/core"
	"youtube-tv/internal/resolver"
)

func splitMaster(prefix string, reversed bool) string {
	low := fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=900000,RESOLUTION=640x360,CODECS=\"avc1,mp4a\",AUDIO=\"low\"\n%svideo360.m3u8\n", prefix)
	high := fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080,CODECS=\"avc1,mp4a\",AUDIO=\"high\"\n%svideo1080.m3u8\n", prefix)
	selected := fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=2500000,RESOLUTION=1280x720,CODECS=\"avc1,mp4a\",AUDIO=\"high\"\n%svideo720.m3u8\n", prefix)
	if reversed {
		low, high = high, low
	}
	return fmt.Sprintf("#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"low\",NAME=\"Low\",DEFAULT=YES,URI=\"%saudio-low.m3u8\"\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"high\",NAME=\"Chinese\",LANGUAGE=\"zh\",DEFAULT=YES,AUTOSELECT=YES,URI=\"%saudio.m3u8\"\n", prefix, prefix) + low + selected + high
}

func TestSelectMasterKeepsOnlySelectedVideoAndAssociatedAudio(t *testing.T) {
	base, _ := url.Parse("https://manifest.googlevideo.com/live/master.m3u8")
	body, tracks, err := selectMasterVariant([]byte(splitMaster("", true)), base, "https://manifest.googlevideo.com/live/video720.m3u8", 720)
	if err != nil || tracks != 1 {
		t.Fatalf("master selection: %d %v", tracks, err)
	}
	text := string(body)
	for _, required := range []string{"#EXTM3U", "#EXT-X-VERSION:6", "#EXT-X-INDEPENDENT-SEGMENTS", `GROUP-ID="high"`, `AUDIO="high"`, "video720.m3u8", `URI="audio.m3u8"`} {
		if !strings.Contains(text, required) {
			t.Errorf("lost master metadata: %s", required)
		}
	}
	for _, removed := range []string{"video1080", "video360", "audio-low", `GROUP-ID="low"`} {
		if strings.Contains(text, removed) {
			t.Errorf("unselected rendition retained: %s", removed)
		}
	}
}

func TestSelectMasterRejectsUnverifiedAudioOrQuality(t *testing.T) {
	base, _ := url.Parse("https://manifest.googlevideo.com/master.m3u8")
	original := splitMaster("", false)
	for name, body := range map[string]string{
		"media-only":      "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nvideo.ts\n",
		"missing-audio":   strings.ReplaceAll(original, `AUDIO="high"`, `AUDIO="absent"`),
		"no-association":  strings.ReplaceAll(original, `,AUDIO="high"`, ""),
		"inband-audio":    strings.ReplaceAll(original, `URI="audio.m3u8"`, `URI=""`),
		"over-quality":    strings.ReplaceAll(original, "1280x720", "1920x1080"),
		"missing-video":   strings.ReplaceAll(original, "video720.m3u8", "other.m3u8"),
		"ambiguous-video": original + "#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720,AUDIO=\"low\"\nvideo720.m3u8\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := selectMasterVariant([]byte(body), base, "https://manifest.googlevideo.com/video720.m3u8", 720); err == nil {
				t.Fatal("unverified split source accepted")
			}
		})
	}
}

func TestIntegrationSplitHLSRelayAndDirect(t *testing.T) {
	for _, mode := range []string{"relay", "direct"} {
		t.Run(mode, func(t *testing.T) {
			var mediaRequests, wrongRequests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("User-Agent") != "Integration/1.0" {
					t.Error("source headers lost")
				}
				switch r.URL.Path {
				case "/entry.m3u8":
					http.Redirect(w, r, "/live/master.m3u8", 302)
				case "/live/master.m3u8":
					fmt.Fprint(w, splitMaster("", false))
				case "/live/audio.m3u8":
					mediaRequests.Add(1)
					fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\naudio.aac\n")
				case "/live/video720.m3u8":
					mediaRequests.Add(1)
					fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6,\nvideo.m4s\n")
				case "/live/audio.aac", "/live/video.m4s", "/live/init.mp4":
					mediaRequests.Add(1)
					fmt.Fprint(w, "payload "+r.URL.Path)
				default:
					wrongRequests.Add(1)
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			srv, local, repo, res := integrationSetup(t, upstream, upstream.URL+"/entry.m3u8")
			res.results[0].VideoURL = upstream.URL + "/live/video720.m3u8"
			resp, master := integrationGet(t, local.Client(), integrationWatch(local, mode), nil)
			if resp.StatusCode != 200 || strings.Count(master, "#EXT-X-STREAM-INF:") != 1 || strings.Contains(master, "1080") || strings.Contains(master, `GROUP-ID="low"`) {
				t.Fatalf("invalid selected master: %d %s", resp.StatusCode, master)
			}
			links := integrationLinks(master)
			if len(links) != 2 {
				t.Fatalf("video or audio missing: %v", links)
			}
			if mode == "direct" {
				if links[0] != upstream.URL+"/live/audio.m3u8" || links[1] != upstream.URL+"/live/video720.m3u8" || strings.Contains(master, "test-playback-token") || mediaRequests.Load() != 0 {
					t.Fatal("direct mode did not keep media at upstream")
				}
				srv.mu.Lock()
				refs := len(srv.resources)
				srv.mu.Unlock()
				if refs != 0 {
					t.Fatal("direct mode created relay references")
				}
				return
			}
			for _, link := range links {
				if !strings.HasPrefix(link, local.URL+"/media/") || strings.Contains(master, upstream.URL) {
					t.Fatal("relay exposed upstream links")
				}
				resp, playlist := integrationGet(t, local.Client(), link, nil)
				if resp.StatusCode != 200 {
					t.Fatalf("track request: %d %s", resp.StatusCode, playlist)
				}
				for _, segment := range integrationLinks(playlist) {
					resp, body := integrationGet(t, local.Client(), segment, nil)
					if resp.StatusCode != 200 || !strings.HasPrefix(body, "payload /live/") {
						t.Fatalf("segment request: %d %s", resp.StatusCode, body)
					}
				}
			}
			if mediaRequests.Load() != 5 || wrongRequests.Load() != 0 {
				t.Fatalf("unexpected downloads: media=%d wrong=%d", mediaRequests.Load(), wrongRequests.Load())
			}
			// Independent audio receives the same authentication and channel checks.
			badToken := strings.ReplaceAll(links[0], "test-playback-token", "invalid")
			if resp, _ := integrationGet(t, local.Client(), badToken, nil); resp.StatusCode != 401 {
				t.Fatal("audio bypassed token validation")
			}
			repo.mu.Lock()
			repo.channels[0].Enabled = false
			repo.mu.Unlock()
			if resp, _ := integrationGet(t, local.Client(), links[0], nil); resp.StatusCode != 410 {
				t.Fatal("disabled channel retained audio access")
			}
		})
	}
}

func TestSplitMasterRejectsForeignAudioInBothModes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.ReplaceAll(splitMaster("", false), `URI="audio.m3u8"`, `URI="http://foreign.invalid/private"`))
	}))
	defer upstream.Close()
	srv, local, _, res := integrationSetup(t, upstream)
	res.results[0].VideoURL = upstream.URL + "/video720.m3u8"
	for _, mode := range []string{"relay", "direct"} {
		resp, body := integrationGet(t, local.Client(), integrationWatch(local, mode), nil)
		if resp.StatusCode != 502 || strings.Contains(body, "foreign.invalid") || srv.Statuses()["stable-one"].State != "error" {
			t.Fatalf("untrusted audio was not rejected: %d %s", resp.StatusCode, body)
		}
	}
}

func TestSplitAudioAndVideoRefreshWithSameMasterAndReorderedVariants(t *testing.T) {
	var expired atomic.Bool
	var wrongRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/old/") && expired.Load() {
			w.WriteHeader(403)
			return
		}
		switch r.URL.Path {
		case "/master.m3u8":
			prefix := "/old/"
			if expired.Load() {
				prefix = "/new/"
			}
			fmt.Fprint(w, splitMaster(prefix, expired.Load()))
		case "/old/audio.m3u8", "/new/audio.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\naudio.aac\n")
		case "/old/video720.m3u8", "/new/video720.m3u8":
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nvideo.m4s\n")
		case "/new/audio.aac", "/new/video.m4s":
			fmt.Fprint(w, "renewed "+r.URL.Path)
		default:
			wrongRequests.Add(1)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	repo := &refreshRepo{ch: core.Channel{ID: "one", Enabled: true}, settings: core.Settings{DefaultMode: "relay", PlaybackToken: "test-token"}}
	res := &refreshResolver{
		current: resolver.Result{URL: upstream.URL + "/master.m3u8", VideoURL: upstream.URL + "/old/video720.m3u8", Height: 720, ExpiresAt: time.Now().Add(time.Hour)},
		next:    resolver.Result{URL: upstream.URL + "/master.m3u8", VideoURL: upstream.URL + "/new/video720.m3u8", Height: 720, ExpiresAt: time.Now().Add(time.Hour)},
	}
	srv := New(repo, res, refreshOptions())
	local := httptest.NewServer(srv.Handler())
	defer local.Close()
	repo.settings.BaseURL = local.URL
	status, master := getRefreshBody(t, local.URL+"/watch/one?token=test-token")
	if status != 200 {
		t.Fatalf("initial split master: %d %s", status, master)
	}
	links := integrationLinks(master)
	if len(links) != 2 {
		t.Fatal("missing split playlists")
	}
	expired.Store(true)
	for i, link := range links {
		status, playlist := getRefreshBody(t, link)
		if status != 200 {
			t.Fatalf("track refresh %d: %d %s", i, status, playlist)
		}
		status, segment := getRefreshBody(t, firstRefreshURI(t, playlist))
		kind := []string{"audio.aac", "video.m4s"}[i]
		if status != 200 || segment != "renewed /new/"+kind {
			t.Fatalf("refreshed wrong track: %d %s", status, segment)
		}
	}
	res.mu.Lock()
	invalidations := res.invalidations
	res.mu.Unlock()
	if invalidations != 1 || wrongRequests.Load() != 0 {
		t.Fatalf("wrong refresh or unused track fetched: invalidations=%d wrong=%d", invalidations, wrongRequests.Load())
	}
}
