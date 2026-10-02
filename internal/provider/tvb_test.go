package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"iptv-manager/internal/core"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type tvbTransport func(*http.Request) (*http.Response, error)

func (f tvbTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
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
				_, err := m.resolve(context.Background(), ch, core.Settings{}, "test-config")
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
	go func() { _, err := m.resolve(context.Background(), ch, core.Settings{}, "test-config"); done <- err }()
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
