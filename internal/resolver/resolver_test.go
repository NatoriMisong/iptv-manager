package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"youtube-tv/internal/core"
)

func testChannel(id string) core.Channel {
	return core.Channel{ID: id, URL: "https://www.youtube.com/watch?v=vr3XyVCR4T0", Quality: 720, Proxy: "inherit"}
}

func liveData(raw string) []byte {
	data, _ := json.Marshal(map[string]any{
		"title": "新闻直播", "is_live": true, "live_status": "is_live",
		"http_headers": map[string]string{"User-Agent": "test-agent"},
		"formats":      []map[string]any{{"url": raw, "protocol": "m3u8_native", "vcodec": "avc1.64001f", "acodec": "mp4a.40.2", "height": 720}},
	})
	return data
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestParseLiveHLSSelectionAndHeaders(t *testing.T) {
	data := []byte(`{"title":"test","live_status":"is_live","http_headers":{"User-Agent":"ua","Cookie":"private","Authorization":"secret","Referer":"https://www.youtube.com/"},"formats":[
		{"url":"https://manifest.googlevideo.com/high.m3u8","protocol":"m3u8_native","height":1080,"vcodec":"avc1","acodec":"mp4a"},
		{"url":"https://manifest.googlevideo.com/silent.m3u8","protocol":"m3u8_native","height":720,"vcodec":"avc1","acodec":"none"},
		{"url":"https://manifest.googlevideo.com/vp9.m3u8","protocol":"m3u8_native","height":720,"vcodec":"vp9","acodec":"opus"},
		{"url":"https://manifest.googlevideo.com/compatible.m3u8","protocol":"m3u8_native","height":480,"vcodec":"avc1","acodec":"mp4a","http_headers":{"User-Agent":"format-agent","Origin":"https://evil.example","Accept":"x\r\ny"}},
		{"url":"https://127.0.0.1/private.m3u8","protocol":"m3u8_native","height":720,"vcodec":"avc1","acodec":"mp4a"}
	]}`)
	now := time.Now()
	result, err := parseResult(data, 720, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.URL, "compatible") || result.Height != 480 {
		t.Fatalf("wrong selected format: %#v", result)
	}
	if result.Headers["User-Agent"] != "format-agent" || result.Headers["Referer"] != "https://www.youtube.com/" {
		t.Fatalf("missing safe headers: %v", result.Headers)
	}
	for _, key := range []string{"Cookie", "Authorization", "Origin", "Accept"} {
		if result.Headers[key] != "" {
			t.Fatalf("unsafe header %s was propagated", key)
		}
	}
	if !result.ExpiresAt.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("unexpected default TTL %v", result.ExpiresAt)
	}
}

func TestRejectOfflineVideoOnlyOverQualityAndForeignMedia(t *testing.T) {
	for _, test := range []struct {
		name, data string
		want       error
	}{
		{"offline", `{"live_status":"was_live","formats":[]}`, ErrOffline},
		{"explicit-not-live", `{"is_live":false}`, ErrOffline},
		{"not-live-status", `{"live_status":"not_live"}`, ErrOffline},
		{"post-live", `{"live_status":"post_live"}`, ErrOffline},
		{"upcoming", `{"live_status":"is_upcoming"}`, ErrOffline},
		{"video-only", `{"is_live":true,"formats":[{"url":"https://manifest.googlevideo.com/live.m3u8","protocol":"m3u8_native","height":720,"vcodec":"avc1","acodec":"none","manifest_url":"https://manifest.googlevideo.com/master.m3u8"}]}`, ErrNoHLS},
		{"too-high", `{"is_live":true,"formats":[{"url":"https://manifest.googlevideo.com/live.m3u8","protocol":"m3u8_native","height":1080,"vcodec":"avc1","acodec":"mp4a"}]}`, ErrNoHLS},
		{"foreign-media", `{"is_live":true,"formats":[{"url":"http://localhost/live.m3u8","protocol":"m3u8_native","height":720,"vcodec":"avc1","acodec":"mp4a"}]}`, ErrNoHLS},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseResult([]byte(test.data), 720, time.Now())
			if !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
		})
	}
}

func TestIncompleteMetadataIsNotReportedAsOffline(t *testing.T) {
	for _, data := range []string{`{"title":"直播频道"}`, `{"title":"直播频道","is_live":null,"live_status":null,"formats":null}`} {
		_, err := parseResult([]byte(data), 720, time.Now())
		if err == nil || errors.Is(err, ErrOffline) || !strings.Contains(err.Error(), "直播状态") {
			t.Fatalf("incomplete metadata incorrectly classified: %v", err)
		}
	}
	r := New(Options{})
	r.run = func(_ context.Context, _ string, args []string) ([]byte, []byte, error) {
		for _, arg := range args {
			if arg == "--ignore-no-formats-error" {
				t.Error("bot verification errors must not be ignored")
			}
		}
		return nil, []byte("ERROR: Sign in to confirm you’re not a bot"), errors.New("exit status 1")
	}
	_, err := r.Resolve(context.Background(), testChannel("one"), core.Settings{})
	if err == nil || errors.Is(err, ErrOffline) || !strings.Contains(err.Error(), "额外验证") {
		t.Fatalf("bot verification incorrectly classified: %v", err)
	}
}

func TestRequestedFormatsDoNotOverrideCompleteHLS(t *testing.T) {
	data := []byte(`{"is_live":true,"protocol":"https+https","vcodec":"av01","acodec":"opus","height":1080,"requested_formats":[{"url":"https://rr.googlevideo.com/video","vcodec":"av01","acodec":"none"},{"url":"https://rr.googlevideo.com/audio","vcodec":"none","acodec":"opus"}],"formats":[{"url":"https://manifest.googlevideo.com/complete.m3u8","protocol":"m3u8_native","height":720,"vcodec":"avc1","acodec":"mp4a"}]}`)
	result, err := parseResult(data, 720, time.Now())
	if err != nil || result.URL != "https://manifest.googlevideo.com/complete.m3u8" {
		t.Fatalf("requested split formats overrode complete HLS: %#v %v", result, err)
	}
}

func TestSourceExpiration(t *testing.T) {
	now := time.Unix(1900000000, 0)
	for _, raw := range []string{
		"https://manifest.googlevideo.com/live.m3u8?expire=1900000600",
		"https://manifest.googlevideo.com/api/manifest/expire/1900000600/file/index.m3u8",
		"https://manifest.googlevideo.com/expire/1900000700/live.m3u8?expires=1900000600",
	} {
		if got := sourceExpiry(raw, now); !got.Equal(time.Unix(1900000600, 0)) {
			t.Fatalf("%s: %v", raw, got)
		}
	}
	if _, err := parseResult(liveData("https://manifest.googlevideo.com/live.m3u8?expire=1800000000"), 720, now); err == nil {
		t.Fatal("accepted expired source")
	}
}

func TestArgumentsProxyAndWarnings(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://must-not-inherit.example:1234")
	for _, test := range []struct{ name, channelProxy, globalProxy, want string }{
		{"direct", "direct", "http://proxy.example:8000", ""},
		{"inherit-empty", "inherit", "", ""},
		{"inherit", "inherit", "socks5://proxy.example:1080", "socks5://proxy.example:1080"},
		{"override", "http://other.example:8000", "http://proxy.example:8000", "http://other.example:8000"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := New(Options{JSRuntime: "deno"})
			var captured []string
			r.run = func(_ context.Context, command string, args []string) ([]byte, []byte, error) {
				captured = append([]string(nil), args...)
				return liveData("https://manifest.googlevideo.com/live.m3u8"), []byte("WARNING: diagnostic, not JSON"), nil
			}
			ch := testChannel("one")
			ch.Proxy = test.channelProxy
			_, err := r.Resolve(context.Background(), ch, core.Settings{UpstreamProxy: test.globalProxy})
			if err != nil {
				t.Fatal(err)
			}
			for flag, want := range map[string]string{"--proxy": test.want, "--js-runtimes": "deno"} {
				found := false
				for i := range captured {
					if captured[i] == flag && i+1 < len(captured) && captured[i+1] == want {
						found = true
					}
				}
				if !found {
					t.Fatalf("argument %s did not match expected value", flag)
				}
			}
			if captured[len(captured)-2] != "--" || captured[len(captured)-1] != ch.URL {
				t.Fatal("URL was not protected by option terminator")
			}
		})
	}
}

func TestCookiesUsePrivateTemporaryCopyAndPreserveOriginal(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("extractor-failure-%v", fail), func(t *testing.T) {
			original := filepath.Join(t.TempDir(), "readonly-cookies.txt")
			contents := []byte("# Netscape HTTP Cookie File\n.youtube.com\tTRUE\t/\tTRUE\t2000000000\tTEST\toriginal-secret\n")
			if err := os.WriteFile(original, contents, 0400); err != nil {
				t.Fatal(err)
			}
			r := New(Options{CookiesFile: original})
			var temporary string
			r.run = func(_ context.Context, _ string, args []string) ([]byte, []byte, error) {
				for i, arg := range args {
					if arg == "--cookies" && i+1 < len(args) {
						temporary = args[i+1]
					}
				}
				if temporary == "" || temporary == original {
					t.Error("original jar passed to extractor")
					return nil, nil, errors.New("unsafe jar path")
				}
				info, err := os.Stat(temporary)
				if err != nil {
					return nil, nil, err
				}
				if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
					t.Errorf("cookie copy permissions %o", info.Mode().Perm())
				}
				copy, err := os.ReadFile(temporary)
				if err != nil || string(copy) != string(contents) {
					t.Errorf("cookie copy did not preserve content: %v", err)
				}
				if err := os.WriteFile(temporary, []byte("updated by yt-dlp"), 0600); err != nil {
					t.Error("extractor could not update temporary cookie jar")
				}
				if fail {
					return nil, nil, errors.New("extractor failed")
				}
				return liveData("https://manifest.googlevideo.com/live.m3u8"), nil, nil
			}
			_, err := r.Resolve(context.Background(), testChannel("one"), core.Settings{})
			if (err != nil) != fail {
				t.Fatalf("unexpected extraction result %v", err)
			}
			if temporary == "" {
				t.Fatal("extractor did not receive a cookie copy")
			}
			if _, err := os.Stat(temporary); !os.IsNotExist(err) {
				t.Fatalf("temporary credentials remain after extraction: %v", err)
			}
			after, err := os.ReadFile(original)
			if err != nil || string(after) != string(contents) {
				t.Fatal("original read-only cookie jar was changed")
			}
		})
	}
}

func TestOversizedAndUnreadableCookiesFailWithoutInvokingExtractor(t *testing.T) {
	large := filepath.Join(t.TempDir(), "large-cookies.txt")
	if err := os.WriteFile(large, make([]byte, (1<<20)+1), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{large, filepath.Join(t.TempDir(), "private-secret-missing.txt")} {
		r := New(Options{CookiesFile: path})
		r.run = func(context.Context, string, []string) ([]byte, []byte, error) {
			t.Error("extractor started with invalid cookies")
			return nil, nil, nil
		}
		_, err := r.Resolve(context.Background(), testChannel("one"), core.Settings{})
		if err == nil || strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "private-secret") {
			t.Fatalf("invalid cookie error was missing or exposed path: %v", err)
		}
	}
}

func TestSingleFlight(t *testing.T) {
	r := New(Options{})
	var calls atomic.Int32
	gate := make(chan struct{})
	r.run = func(ctx context.Context, _ string, _ []string) ([]byte, []byte, error) {
		calls.Add(1)
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		return liveData("https://manifest.googlevideo.com/live.m3u8"), nil, nil
	}
	const count = 24
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Resolve(context.Background(), testChannel("one"), core.Settings{})
			errs <- err
		}()
	}
	waitFor(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, f := range r.inflight {
			return f.waiters == count
		}
		return false
	})
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("launched %d processes", calls.Load())
	}
}

func TestGlobalSingleProcessAndBoundedQueue(t *testing.T) {
	r := New(Options{})
	var active, maximum atomic.Int32
	gate := make(chan struct{})
	r.run = func(ctx context.Context, _ string, _ []string) ([]byte, []byte, error) {
		n := active.Add(1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		defer active.Add(-1)
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		return liveData("https://manifest.googlevideo.com/live.m3u8"), nil, nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			_, err := r.Resolve(context.Background(), testChannel(fmt.Sprint(id)), core.Settings{})
			errs <- err
		}(i)
	}
	waitFor(t, func() bool { return len(r.queue) == 8 })
	if _, err := r.Resolve(context.Background(), testChannel("overflow"), core.Settings{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("expected bounded queue error, got %v", err)
	}
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if maximum.Load() != 1 {
		t.Fatalf("maximum process count %d", maximum.Load())
	}
}

func TestCancellationDoesNotCancelOtherViewer(t *testing.T) {
	r := New(Options{})
	gate := make(chan struct{})
	r.run = func(ctx context.Context, _ string, _ []string) ([]byte, []byte, error) {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		return liveData("https://manifest.googlevideo.com/live.m3u8"), nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, err := r.Resolve(ctx, testChannel("one"), core.Settings{}); first <- err }()
	go func() { _, err := r.Resolve(context.Background(), testChannel("one"), core.Settings{}); second <- err }()
	waitFor(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, f := range r.inflight {
			return f.waiters == 2
		}
		return false
	})
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	close(gate)
	if err := <-second; err != nil {
		t.Fatal(err)
	}
}

func TestLastCancellationStopsProcess(t *testing.T) {
	r := New(Options{})
	started, stopped := make(chan struct{}), make(chan struct{})
	r.run = func(ctx context.Context, _ string, _ []string) ([]byte, []byte, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := r.Resolve(ctx, testChannel("one"), core.Settings{}); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("process did not stop")
	}
	if status := r.Statuses()["one"]; status.State != "unknown" {
		t.Fatalf("unexpected status %#v", status)
	}
}

func TestCacheRefreshFailureBackoffAndHeaderIsolation(t *testing.T) {
	r := New(Options{})
	var seconds atomic.Int64
	seconds.Store(1900000000)
	r.now = func() time.Time { return time.Unix(seconds.Load(), 0) }
	var calls atomic.Int32
	var fail atomic.Bool
	r.run = func(_ context.Context, _ string, _ []string) ([]byte, []byte, error) {
		calls.Add(1)
		if fail.Load() {
			return nil, []byte("secret-user:secret-password signed-url"), errors.New("failure")
		}
		return liveData(fmt.Sprintf("https://manifest.googlevideo.com/live.m3u8?expire=%d", seconds.Load()+300)), nil, nil
	}
	ch := testChannel("one")
	first, err := r.Resolve(context.Background(), ch, core.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	first.Headers["User-Agent"] = "mutated"
	second, err := r.Resolve(context.Background(), ch, core.Settings{})
	if err != nil || second.Headers["User-Agent"] != "test-agent" || calls.Load() != 1 {
		t.Fatal("cache was not isolated or reused")
	}
	seconds.Add(240)
	if _, err := r.Resolve(context.Background(), ch, core.Settings{}); err != nil || calls.Load() != 2 {
		t.Fatalf("did not refresh before expiry: %v", err)
	}
	r.Invalidate("one")
	fail.Store(true)
	for i := 0; i < 2; i++ {
		_, err := r.Resolve(context.Background(), ch, core.Settings{})
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("failure not sanitized")
		}
	}
	if calls.Load() != 3 {
		t.Fatal("failure was not backed off")
	}
	seconds.Add(16)
	_, _ = r.Resolve(context.Background(), ch, core.Settings{})
	if calls.Load() != 4 {
		t.Fatal("failure backoff did not expire")
	}
}

func TestInvalidationPreventsOldInFlightCache(t *testing.T) {
	r := New(Options{})
	gate := make(chan struct{})
	var calls atomic.Int32
	r.run = func(_ context.Context, _ string, _ []string) ([]byte, []byte, error) {
		if calls.Add(1) == 1 {
			<-gate
			return liveData("https://manifest.googlevideo.com/old.m3u8"), nil, nil
		}
		return liveData("https://manifest.googlevideo.com/new.m3u8"), nil, nil
	}
	done := make(chan error, 1)
	go func() { _, err := r.Resolve(context.Background(), testChannel("one"), core.Settings{}); done <- err }()
	waitFor(t, func() bool { return calls.Load() == 1 })
	r.Invalidate("one")
	close(gate)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("invalidated call returned %v", err)
	}
	result, err := r.Resolve(context.Background(), testChannel("one"), core.Settings{})
	if err != nil || !strings.Contains(result.URL, "new.m3u8") {
		t.Fatalf("stale cache survived: %#v %v", result, err)
	}
	if r.Statuses()["one"].State != "ready" {
		t.Fatal("new status not ready")
	}
}

func TestSourceAndProxyChangesDoNotReuseCache(t *testing.T) {
	r := New(Options{})
	var calls atomic.Int32
	r.run = func(_ context.Context, _ string, _ []string) ([]byte, []byte, error) {
		calls.Add(1)
		return liveData("https://manifest.googlevideo.com/live.m3u8"), nil, nil
	}
	ch := testChannel("one")
	_, _ = r.Resolve(context.Background(), ch, core.Settings{})
	ch.URL = "https://www.youtube.com/watch?v=V1p33hqPrUk"
	_, _ = r.Resolve(context.Background(), ch, core.Settings{})
	ch.Proxy = "http://proxy.example:8080"
	_, _ = r.Resolve(context.Background(), ch, core.Settings{})
	ch.Quality = 1080
	_, _ = r.Resolve(context.Background(), ch, core.Settings{})
	if calls.Load() != 4 {
		t.Fatalf("source/proxy/quality shared cache: %d", calls.Load())
	}
}

func TestUnsafeSourceNeverInvokesExtractor(t *testing.T) {
	r := New(Options{})
	r.run = func(context.Context, string, []string) ([]byte, []byte, error) {
		t.Error("extractor invoked")
		return nil, nil, nil
	}
	for _, raw := range []string{"--exec=touch /tmp/pwn", "https://youtube.com.evil.example/x", "http://127.0.0.1", "https://user:pass@youtube.com/watch?v=test", "https://www.youtube.com:8443/watch?v=test"} {
		ch := testChannel("one")
		ch.URL = raw
		if _, err := r.Resolve(context.Background(), ch, core.Settings{}); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestCommandSeparatesStderrAndCapsBuffers(t *testing.T) {
	t.Setenv("GO_WANT_RESOLVER_HELPER", "1")
	stdout, stderr, err := runCommand(context.Background(), os.Args[0], []string{"-test.run=TestResolverHelperProcess", "--", "warning"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseResult(stdout, 720, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stderr), "WARNING") {
		t.Fatal("stderr not separated")
	}
	bounded := &boundedBuffer{limit: 5}
	if n, err := bounded.Write([]byte("123456789")); n != 9 || err != nil {
		t.Fatal("did not consume full write")
	}
	_, _ = bounded.Write([]byte("more"))
	if !bounded.overflow || bounded.Len() != 5 {
		t.Fatal("output cap failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err = runCommand(ctx, os.Args[0], []string{"-test.run=TestResolverHelperProcess", "--", "block"})
	if err == nil || ctx.Err() == nil {
		t.Fatal("command timeout did not interrupt process")
	}
}

func TestResolverHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_RESOLVER_HELPER") != "1" {
		return
	}
	if os.Args[len(os.Args)-1] == "block" {
		time.Sleep(time.Hour)
	}
	fmt.Fprintln(os.Stderr, "WARNING: harmless extractor diagnostic")
	fmt.Print(string(liveData("https://manifest.googlevideo.com/live.m3u8")))
	os.Exit(0)
}
