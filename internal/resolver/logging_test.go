package resolver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"iptv-manager/internal/core"
)

func loggingResolver(buffer *bytes.Buffer) *Resolver {
	return New(Options{Logger: slog.New(slog.NewJSONHandler(buffer, &slog.HandlerOptions{Level: slog.LevelDebug}))})
}

func readLogRecords(t *testing.T, buffer *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(buffer.Bytes()), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func TestResolutionLogsSuccessAndCacheWithoutSignedURL(t *testing.T) {
	var buffer bytes.Buffer
	r := loggingResolver(&buffer)
	r.run = func(context.Context, string, []string) ([]byte, []byte, error) {
		return liveData("https://manifest.googlevideo.com/live.m3u8?signature=secret-signature"), nil, nil
	}
	for range 2 {
		if _, err := r.Resolve(context.Background(), testChannel("channel-one"), core.Settings{}); err != nil {
			t.Fatal(err)
		}
	}
	counts := map[string]int{}
	for _, record := range readLogRecords(t, &buffer) {
		counts[record["msg"].(string)]++
		if record["msg"] == "直播来源解析成功" {
			if record["channel"] != "channel-one" || record["video"] != "vr3XyVCR4T0" || record["height"] != float64(720) || record["source_host"] != "manifest.googlevideo.com" || record["duration_ms"] == nil || record["expires_at"] == nil {
				t.Fatalf("missing success context: %v", record)
			}
		}
	}
	for _, message := range []string{"直播来源解析开始", "直播来源解析成功", "命中直播来源缓存"} {
		if counts[message] != 1 {
			t.Fatalf("unexpected event counts: %v", counts)
		}
	}
	if strings.Contains(buffer.String(), "secret-signature") || strings.Contains(buffer.String(), "live.m3u8") {
		t.Fatal("signed media URL leaked to logs")
	}
}

func TestNoHLSLogsExplainExistingRejections(t *testing.T) {
	var buffer bytes.Buffer
	r := loggingResolver(&buffer)
	r.run = func(context.Context, string, []string) ([]byte, []byte, error) {
		return []byte(`{"is_live":true,"formats":[
			{"format_id":"high","url":"https://manifest.googlevideo.com/high.m3u8","protocol":"m3u8_native","height":1080,"vcodec":"avc1","acodec":"mp4a"},
			{"format_id":"silent","url":"https://manifest.googlevideo.com/silent.m3u8","protocol":"m3u8_native","height":720,"vcodec":"avc1","acodec":"none"},
			{"format_id":"audio","url":"https://manifest.googlevideo.com/audio.m3u8","protocol":"m3u8_native","height":0,"vcodec":"none","acodec":"mp4a"},
			{"format_id":"unknown-height","url":"https://manifest.googlevideo.com/unknown.m3u8","protocol":"m3u8_native","vcodec":"avc1","acodec":"mp4a"},
			{"format_id":"dash","url":"https://rr.googlevideo.com/video?token=hidden","protocol":"http_dash_segments","height":720,"vcodec":"avc1","acodec":"none"},
			{"format_id":"invalid","url":"http://localhost/internal","protocol":"m3u8_native","height":720,"vcodec":"avc1","acodec":"mp4a"}
		]}`), []byte("WARNING: Some formats have been skipped"), nil
	}
	_, err := r.Resolve(context.Background(), testChannel("one"), core.Settings{})
	if !errors.Is(err, ErrNoHLS) {
		t.Fatalf("selection behavior changed: %v", err)
	}
	foundSummary, foundError := false, false
	for _, record := range readLogRecords(t, &buffer) {
		if record["msg"] == "HLS 格式筛选结果" {
			foundSummary = true
			if record["formats_total"] != float64(6) || record["hls_total"] != float64(5) || record["quality_limit"] != float64(720) {
				t.Fatalf("incorrect format counts: %v", record)
			}
			counts := record["rejections"].(map[string]any)
			for _, reason := range []string{"above_quality_limit", "missing_audio_master", "missing_video_codec", "unknown_height", "not_hls", "invalid_media_url"} {
				if counts[reason] != float64(1) {
					t.Fatalf("missing rejection %s: %v", reason, counts)
				}
			}
		}
		if record["msg"] == "直播来源解析失败" && record["error"] == ErrNoHLS.Error() {
			foundError = true
		}
	}
	if !foundSummary || !foundError || !strings.Contains(buffer.String(), "Some formats have been skipped") {
		t.Fatal("missing format summary, yt-dlp warning, or final error")
	}
	if strings.Contains(buffer.String(), "token=hidden") || strings.Contains(buffer.String(), "localhost/internal") {
		t.Fatal("format URL leaked")
	}
}

func TestExtractorFailureLogsUsefulSanitizedDiagnostics(t *testing.T) {
	var buffer bytes.Buffer
	r := loggingResolver(&buffer)
	r.run = func(context.Context, string, []string) ([]byte, []byte, error) {
		return nil, []byte("ERROR: Sign in to confirm you’re not a bot; HTTP Error 403\nproxy http://proxy-user:proxy-password@proxy.example:8080\nCookie: SID=cookie-secret\nAuthorization: Bearer bearer-secret\nsource https://manifest.googlevideo.com/expire/999/signature/hidden-signature/index.m3u8\ntoken=token-secret"), errors.New("failed process")
	}
	ch := testChannel("one")
	ch.Proxy = "0123456789abcdef01234567"
	settings := core.Settings{Proxies: []core.Proxy{{ID: ch.Proxy, Name: "代理", Scheme: "http", Host: "proxy.example", Port: 8080, Username: "proxy-user", Password: "proxy-password"}}}
	_, err := r.Resolve(context.Background(), ch, settings)
	if err == nil || !strings.Contains(err.Error(), "额外验证") {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, value := range []string{"Sign in to confirm", "HTTP Error 403", "yt-dlp 执行失败", "直播来源解析失败"} {
		if !strings.Contains(buffer.String(), value) {
			t.Errorf("missing diagnostic %q", value)
		}
	}
	for _, secret := range []string{"proxy-user", "proxy-password", "cookie-secret", "bearer-secret", "hidden-signature", "token-secret"} {
		if strings.Contains(buffer.String(), secret) {
			t.Errorf("secret leaked: %s", secret)
		}
	}
}

func TestDiagnosticRedactionAndBounds(t *testing.T) {
	raw := "\x1b[31mERROR: Requested format is not available\x1b[0m\n" +
		"--cookies '/private/cookie-jar.txt' --password=plain-password\n" +
		"signature=standalone-secret\n" +
		"Proxy-Authorization: Basic header-secret\n" +
		"https://manifest.googlevideo.com/" + strings.Repeat("url-secret", 2000) + "\n" +
		strings.Repeat("bounded diagnostic ", 2000)
	got := sanitizeDiagnostic(raw)
	if !strings.Contains(got, "Requested format is not available") || !strings.HasSuffix(got, "[truncated]") || len([]rune(got)) > 8250 {
		t.Fatal("diagnostic lost useful context or exceeded its bound")
	}
	for _, secret := range []string{"cookie-jar", "plain-password", "standalone-secret", "header-secret", "url-secret", "\x1b"} {
		if strings.Contains(got, secret) {
			t.Errorf("sensitive diagnostic was not removed: %s", secret)
		}
	}
}

func TestResolutionTimeoutLogsFailure(t *testing.T) {
	var buffer bytes.Buffer
	r := loggingResolver(&buffer)
	r.opts.Timeout = 20 * time.Millisecond
	r.run = func(ctx context.Context, _ string, _ []string) ([]byte, []byte, error) {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	_, err := r.Resolve(context.Background(), testChannel("one"), core.Settings{})
	if err == nil || !strings.Contains(err.Error(), "超时") || !strings.Contains(buffer.String(), "直播来源解析失败") {
		t.Fatalf("timeout misreported: %v", err)
	}
	if r.Statuses()["one"].UpdatedAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatal("channel status was not updated")
	}
}
