package media

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"iptv-manager/internal/core"
	"iptv-manager/internal/resolver"
)

func identityMaster(video string) string {
	return strings.ReplaceAll(strings.ReplaceAll(splitMaster("", true), "video720.m3u8", video), `CODECS="avc1,mp4a",AUDIO="high"`, `CODECS="avc1.4D401F,mp4a.40.2",FRAME-RATE=29.970,AUDIO="high"`)
}

func TestMasterMatchesRenewedYouTubeRendition(t *testing.T) {
	base, _ := url.Parse("https://manifest.googlevideo.com/master.m3u8")
	format := resolver.VideoFormat{ID: "232-1", Codec: "avc1.4D401F", Width: 1280, FPS: 29.97}
	for _, tc := range []struct{ name, old, current string }{
		{"path-signatures-and-host", "https://old.googlevideo.com/api/manifest/hls_playlist/expire/1900000000/id/broadcast.1/itag/232/signature/old/file/index.m3u8", "https://new.googlevideo.com/api/manifest/hls_playlist/expire/1900001000/id/broadcast.1/itag/232/signature/new/file/index.m3u8"},
		{"query-order", "https://manifest.googlevideo.com/video.m3u8?id=broadcast&itag=232&sig=same", "https://manifest.googlevideo.com/video.m3u8?sig=same&itag=232&id=broadcast"},
		{"query-signature", "https://manifest.googlevideo.com/video.m3u8?itag=232&id=broadcast&sig=old", "https://manifest.googlevideo.com/video.m3u8?itag=232&id=broadcast&sig=new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A same-height alternative is not a replacement for the chosen itag.
			master := identityMaster(tc.current) + "#EXT-X-STREAM-INF:RESOLUTION=1280x720,CODECS=\"vp09.00.31.08,mp4a\",AUDIO=\"low\"\nhttps://manifest.googlevideo.com/video.m3u8?itag=999\n"
			body, info, err := selectMasterVariant([]byte(master), base, tc.old, 720, format)
			if err != nil || info.Match != "youtube_itag" || info.Candidates != 1 || info.AudioTracks != 1 || info.Variants != 4 {
				t.Fatalf("renewed rendition: %+v %v", info, err)
			}
			if !strings.Contains(string(body), tc.current) || strings.Contains(string(body), tc.old) || strings.Contains(string(body), `GROUP-ID="low"`) || strings.Contains(string(body), "1080") || strings.Contains(string(body), "itag=999") {
				t.Fatalf("wrong video/audio selection: %s", body)
			}
		})
	}
}

func TestMasterIdentityDoesNotGuessOrBypassMetadata(t *testing.T) {
	base, _ := url.Parse("https://manifest.googlevideo.com/master.m3u8")
	old := "https://manifest.googlevideo.com/video.m3u8?itag=232&id=broadcast&sig=old"
	current := "https://manifest.googlevideo.com/video.m3u8?itag=232&id=broadcast&sig=new"
	format := resolver.VideoFormat{Codec: "avc1.4D401F", Width: 1280, FPS: 29.97}
	master := identityMaster(current)
	for name, body := range map[string]string{
		"different-itag":              strings.ReplaceAll(master, "itag=232", "itag=999"),
		"different-content":           strings.ReplaceAll(master, "id=broadcast", "id=other"),
		"missing-content":             strings.ReplaceAll(master, "&id=broadcast", ""),
		"duplicate-itag":              strings.ReplaceAll(master, "itag=232", "itag=232&itag=233"),
		"same-height-different-codec": strings.ReplaceAll(master, "avc1.4D401F", "vp09.00.31.08"),
		"different-width":             strings.ReplaceAll(master, "1280x720", "960x720"),
		"different-height":            strings.ReplaceAll(master, "1280x720", "1920x1080"),
		"different-fps":               strings.ReplaceAll(master, "29.970", "59.940"),
		"unknown-fps":                 strings.ReplaceAll(master, "29.970", "NaN"),
		"missing-codecs":              strings.ReplaceAll(master, `CODECS="avc1.4D401F,mp4a.40.2",`, ""),
		"same-url-wrong-codec":        strings.ReplaceAll(strings.ReplaceAll(master, current, old), "avc1.4D401F", "vp09.00.31.08"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := selectMasterVariant([]byte(body), base, old, 720, format); !errors.Is(err, errMasterSelection) {
				t.Fatalf("mismatched rendition accepted: %v", err)
			}
		})
	}
	if _, _, err := selectMasterVariant([]byte(master), base, old, 720, resolver.VideoFormat{}); !errors.Is(err, errMasterSelection) {
		t.Fatal("matched by resolution/itag without codec metadata")
	}
	ambiguous := master + fmt.Sprintf("#EXT-X-STREAM-INF:RESOLUTION=1280x720,CODECS=\"avc1.4D401F,mp4a.40.2\",FRAME-RATE=29.970,AUDIO=\"low\"\n%s&duplicate=1\n", current)
	if _, info, err := selectMasterVariant([]byte(ambiguous), base, old, 720, format); err == nil || errors.Is(err, errMasterSelection) || info.Candidates != 2 {
		t.Fatalf("ambiguous match must fail without automatic retry: %+v %v", info, err)
	}
}

func TestIntegrationRenewedMasterURLsInBothModes(t *testing.T) {
	for _, mode := range []string{"relay", "direct"} {
		t.Run(mode, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/master.m3u8" {
					fmt.Fprint(w, strings.ReplaceAll(identityMaster("video720.m3u8?itag=232&sig=new"), `URI="audio.m3u8"`, `URI="audio.m3u8?sig=new"`))
					return
				}
				if r.URL.Query().Get("sig") != "new" {
					t.Error("stale signature used for media")
					w.WriteHeader(403)
					return
				}
				switch r.URL.Path {
				case "/video720.m3u8", "/audio.m3u8":
					fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\n%s.ts?sig=new\n", strings.TrimSuffix(r.URL.Path, ".m3u8"))
				case "/video720.ts", "/audio.ts":
					fmt.Fprint(w, "payload "+r.URL.Path)
				default:
					t.Error("unselected rendition fetched")
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			_, local, repo, res := integrationSetup(t, upstream)
			res.results[0].VideoURL = upstream.URL + "/video720.m3u8?itag=232&sig=old"
			res.results[0].VideoFormat = resolver.VideoFormat{Codec: "avc1.4D401F", Width: 1280, FPS: 29.97}
			resp, master := integrationGet(t, local.Client(), integrationWatch(repo, local, mode), nil)
			if resp.StatusCode != 200 || strings.Contains(master, "sig=old") || strings.Count(master, "#EXT-X-STREAM-INF:") != 1 {
				t.Fatalf("failed to select renewed master: %d %s", resp.StatusCode, master)
			}
			links := integrationLinks(master)
			if len(links) != 2 {
				t.Fatal("missing associated audio/video")
			}
			for _, link := range links {
				if (mode == "relay") != strings.HasPrefix(link, local.URL+"/media/") {
					t.Fatal("wrong playback mode")
				}
				resp, body := integrationGet(t, local.Client(), link, nil)
				if resp.StatusCode != 200 {
					t.Fatalf("track failed: %d", resp.StatusCode)
				}
				base, _ := url.Parse(link)
				relative, _ := url.Parse(firstRefreshURI(t, body))
				resp, body = integrationGet(t, local.Client(), base.ResolveReference(relative).String(), nil)
				if resp.StatusCode != 200 || !strings.HasPrefix(body, "payload /") {
					t.Fatalf("media failed: %d %s", resp.StatusCode, body)
				}
			}
			if res.invalidations != 0 || res.calls != 1 {
				t.Fatal("signature drift unnecessarily re-ran resolver")
			}
		})
	}
}

func TestMasterSelectionRefreshIsBoundedAndRecovers(t *testing.T) {
	for _, mode := range []string{"relay", "direct"} {
		for _, recovers := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/recovery=%t", mode, recovers), func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprint(w, splitMaster("/current/", true))
				}))
				defer upstream.Close()
				repo := &refreshRepo{ch: core.Channel{ID: "one", Enabled: true, Mode: mode}, settings: core.Settings{PlaybackToken: "test-token"}}
				result := resolver.Result{URL: upstream.URL + "/master.m3u8", VideoURL: upstream.URL + "/old/video720.m3u8", Height: 720, ExpiresAt: time.Now().Add(time.Hour), VideoFormat: resolver.VideoFormat{Codec: "avc1", Width: 1280}}
				res := &refreshResolver{current: result, next: result}
				if recovers {
					res.current.VideoFormat.Codec = "avc1.old"
					res.next.VideoURL = upstream.URL + "/current/video720.m3u8"
				}
				srv := New(repo, res, refreshOptions())
				local := httptest.NewServer(srv.Handler())
				defer local.Close()
				repo.settings.BaseURL = local.URL
				watch := local.URL + "/watch/one?token=test-token"
				status, master := getRefreshBody(t, watch)
				if recovers {
					if status != 200 || len(integrationLinks(master)) != 2 || srv.Statuses()["one"].State == "error" {
						t.Fatalf("did not recover cleanly: %d %s", status, master)
					}
				} else if status != 502 {
					t.Fatalf("unbounded/unsafe retry: %d", status)
				}
				// Concurrent VLC retries reuse the renewed result or hit the cooldown.
				var group sync.WaitGroup
				for i := 0; i < 4; i++ {
					group.Add(1)
					go func() {
						defer group.Done()
						resp, _ := integrationGet(t, local.Client(), watch, nil)
						if recovers && resp.StatusCode != 200 || !recovers && (resp.StatusCode != 503 || resp.Header.Get("Retry-After") != "30") {
							t.Errorf("retry status = %d", resp.StatusCode)
						}
					}()
				}
				group.Wait()
				res.mu.Lock()
				count := res.invalidations
				res.mu.Unlock()
				if count != 1 {
					t.Fatalf("resolver invalidated %d times", count)
				}
				if !recovers {
					// Both expiry of the cooldown and explicit manual refresh unblock it.
					srv.mu.Lock()
					srv.selectionRefreshes["one"] = time.Now().Add(-time.Second)
					srv.mu.Unlock()
					if status, _ := getRefreshBody(t, watch); status != 502 {
						t.Fatal("cooldown did not expire")
					}
					srv.Invalidate("one")
					if status, _ := getRefreshBody(t, watch); status != 502 {
						t.Fatal("manual refresh blocked by cooldown")
					}
				}
			})
		}
	}
}

func TestRenewedMasterStillValidatesVideoHost(t *testing.T) {
	base, _ := url.Parse("https://manifest.googlevideo.com/master.m3u8")
	ref := resource{URL: base.String(), RootURL: base.String(), Channel: "test", VideoURL: "https://manifest.googlevideo.com/video.m3u8?itag=232&sig=old", Height: 720, VideoFormat: resolver.VideoFormat{Codec: "avc1.4D401F"}}
	srv := New(nil, nil, Options{})
	for _, host := range []string{"https://foreign.invalid", "http://127.0.0.1"} {
		if _, err := srv.prepareManifest([]byte(identityMaster(host+"/video.m3u8?itag=232&sig=new")), base, ref); err == nil || errors.Is(err, errMasterSelection) {
			t.Fatalf("renewed URL guard bypassed: %v", err)
		}
	}
}

func TestMasterDiagnosticsDoNotExposeSignedURLs(t *testing.T) {
	var logs bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(original)
	base, _ := url.Parse("https://manifest.googlevideo.com/master.m3u8?sig=secret_master")
	ref := resource{URL: base.String(), RootURL: base.String(), Channel: "test", VideoURL: "https://manifest.googlevideo.com/video.m3u8?itag=232&sig=secret_old", Height: 720, VideoFormat: resolver.VideoFormat{Codec: "avc1.4D401F"}}
	srv := New(nil, nil, Options{})
	if _, err := srv.prepareManifest([]byte(identityMaster("video.m3u8?itag=232&sig=secret_new")), base, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.prepareManifest([]byte(identityMaster("video.m3u8?itag=999&sig=secret_new")), base, ref); !errors.Is(err, errMasterSelection) {
		t.Fatalf("expected selection diagnostic: %v", err)
	}
	for _, required := range []string{"match=youtube_itag", "variants=3", "selected_itag=232", "refreshable=true"} {
		if !strings.Contains(logs.String(), required) {
			t.Errorf("missing diagnostic %q", required)
		}
	}
	if strings.Contains(logs.String(), "secret_") || strings.Contains(logs.String(), "https://") {
		t.Fatal("signed URL exposed in diagnostics")
	}
}
