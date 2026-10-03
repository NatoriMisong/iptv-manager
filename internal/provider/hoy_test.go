package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"iptv-manager/internal/core"
)

func hoyTestPolicy(resource string, until time.Time) string {
	p := fmt.Sprintf(`{"Statement":[{"Resource":%q,"Condition":{"DateLessThan":{"AWS:EpochTime":%d}}}]}`, resource, until.Unix())
	return strings.NewReplacer("+", "-", "=", "_", "/", "~").Replace(base64.StdEncoding.EncodeToString([]byte(p)))
}

func hoyTestData(now time.Time) map[string]any {
	return map[string]any{"id": 1, "video": map[string]any{"id": 76, "link": hoyRoot("76") + "index-fhd.m3u8"}, "signed": hoySignature{Policy: hoyTestPolicy(hoyRoot("76")+"*", now.Add(2*time.Hour)), KeyPairID: "test-key", Signature: "private-signature"}}
}

func hoyTestResponse(r *http.Request, data map[string]any) *http.Response {
	w := httptest.NewRecorder()
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": data})
	resp := w.Result()
	resp.Request = r
	return resp
}

func hoyTestChannel() core.Channel {
	return core.Channel{ID: "saved-76", SourceType: "builtin", ProviderID: "hoy", ProviderChannelID: "76"}
}

func TestHOYInvalidCheckoutIsSanitizedAndNegativelyCached(t *testing.T) {
	now := time.Now()
	for _, kind := range []string{"foreign", "wrong-directory", "wrong-channel", "wrong-id", "not-hls", "missing-signature", "bad-policy", "wrong-scope", "expired", "nearly-expired"} {
		t.Run(kind, func(t *testing.T) {
			data := hoyTestData(now)
			video := data["video"].(map[string]any)
			sig := data["signed"].(hoySignature)
			switch kind {
			case "foreign":
				video["link"] = "https://evil.invalid/master.m3u8?private-token=value"
			case "wrong-directory":
				video["link"] = "https://ch76-live-stream.hoy.tv/ch77/index.m3u8"
			case "wrong-channel":
				video["id"] = 77
			case "wrong-id":
				data["id"] = 2
			case "not-hls":
				video["link"] = hoyRoot("76") + "index.mpd"
			case "missing-signature":
				sig.Signature = ""
			case "bad-policy":
				sig.Policy = "private-invalid-policy"
			case "wrong-scope":
				sig.Policy = hoyTestPolicy(hoyRoot("77")+"*", now.Add(time.Hour))
			case "expired":
				sig.Policy = hoyTestPolicy(hoyRoot("76")+"*", now.Add(-time.Second))
			case "nearly-expired":
				sig.Policy = hoyTestPolicy(hoyRoot("76")+"*", now.Add(20*time.Second))
			}
			data["signed"] = sig
			var calls atomic.Int32
			m := newHOYResolver(func(string) (*http.Client, error) {
				return &http.Client{Transport: tvbTransport(func(r *http.Request) (*http.Response, error) {
					calls.Add(1)
					return hoyTestResponse(r, data), nil
				})}, nil
			})
			m.now = func() time.Time { return now }
			defer m.Invalidate(hoyTestChannel().ID)
			for i := 0; i < 2; i++ {
				_, err := m.Resolve(context.Background(), hoyTestChannel(), core.Settings{}, "config")
				if err == nil || strings.Contains(err.Error(), "private-") {
					t.Fatalf("unsafe checkout accepted or exposed: %v", err)
				}
			}
			if calls.Load() != 1 {
				t.Fatal("failure not cached")
			}
		})
	}
}

func TestHOYSignatureScopeAndQueryReplacement(t *testing.T) {
	e := &hoySession{code: "76", signed: hoySignature{Policy: "new-policy", KeyPairID: "new-key", Signature: "new-signature"}}
	for _, raw := range []string{
		"http://ch76-live-stream.hoy.tv/ch76/a.ts", "https://ch76-live-stream.hoy.tv.evil.test/ch76/a.ts",
		"https://ch77-live-stream.hoy.tv/ch77/a.ts", "https://ch76-live-stream.hoy.tv/ch77/a.ts",
		"https://ch76-live-stream.hoy.tv/ch76/../ch77/a.ts", "https://ch76-live-stream.hoy.tv/ch76/%2e%2e/ch77/a.ts",
		"https://ch76-live-stream.hoy.tv/ch76/%252e%252e/ch77/a.ts", "https://ch76-live-stream.hoy.tv:8443/ch76/a.ts",
		"https://user@ch76-live-stream.hoy.tv/ch76/a.ts", "https://ch76-live-stream.hoy.tv/ch76/a.ts#fragment",
		"https://api2.hoy.tv/api/v3/a/liveCheckout/1", hoyRoot("76") + "a.ts?bad=%GG",
	} {
		if _, err := e.signedURL(raw); err == nil {
			t.Fatalf("unsafe signing destination accepted: %s", raw)
		}
	}
	for _, file := range []string{"video.m3u8", "audio.aac", "segment.ts", "init.mp4", "key.bin", "subtitle.vtt"} {
		raw, err := e.signedURL(hoyRoot("76") + file + "?keep=a%2Bb&Policy=old&Signature=old&Signature=older")
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(raw)
		q := u.Query()
		if q.Get("keep") != "a+b" || q.Get("Signature") != "new-signature" || len(q["Signature"]) != 1 || q.Get("Policy") != "new-policy" || q.Get("Key-Pair-Id") != "new-key" {
			t.Fatal("signature not replaced or query lost")
		}
	}
}

func TestHOYRedirectsStayInChannelAndReapplySignature(t *testing.T) {
	for _, target := range []string{hoyRoot("76") + "redirected.ts", hoyRoot("77") + "a.ts", "https://evil.invalid/a.ts", "https://ch76-live-stream.hoy.tv/outside/a.ts"} {
		t.Run(target, func(t *testing.T) {
			var mediaCalls atomic.Int32
			m := newHOYResolver(func(string) (*http.Client, error) {
				return &http.Client{Transport: tvbTransport(func(r *http.Request) (*http.Response, error) {
					if r.URL.Hostname() == "api2.hoy.tv" {
						return hoyTestResponse(r, hoyTestData(time.Now())), nil
					}
					if mediaCalls.Add(1) == 1 {
						return &http.Response{StatusCode: 302, Header: http.Header{"Location": {target}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
					}
					if r.URL.String() == target || r.URL.Query().Get("Signature") != "private-signature" {
						t.Error("redirect lost signature")
					}
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("media")), Request: r}, nil
				})}, nil
			})
			defer m.Invalidate(hoyTestChannel().ID)
			p, err := m.Resolve(context.Background(), hoyTestChannel(), core.Settings{}, "config")
			if err != nil {
				t.Fatal(err)
			}
			resp, err := p.Session.Fetch(context.Background(), hoyRoot("76")+"first.ts", "")
			if resp != nil {
				resp.Body.Close()
			}
			allowed := strings.HasSuffix(target, "redirected.ts")
			if (err == nil) != allowed || (!allowed && mediaCalls.Load() != 1) {
				t.Fatalf("redirect result: %v calls=%d", err, mediaCalls.Load())
			}
		})
	}
}

func TestHOYRefreshCancelsPendingCheckout(t *testing.T) {
	started := make(chan struct{})
	m := newHOYResolver(func(string) (*http.Client, error) {
		return &http.Client{Transport: tvbTransport(func(r *http.Request) (*http.Response, error) {
			close(started)
			<-r.Context().Done()
			// Even an upstream that returns data after cancellation cannot restore it.
			return hoyTestResponse(r, hoyTestData(time.Now())), nil
		})}, nil
	})
	done := make(chan error, 1)
	go func() {
		_, err := m.Resolve(context.Background(), hoyTestChannel(), core.Settings{}, "config")
		done <- err
	}()
	<-started
	m.Invalidate(hoyTestChannel().ID)
	select {
	case err := <-done:
		if !errors.Is(err, ErrRevoked) {
			t.Fatalf("cleared checkout restored: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending checkout not canceled")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) != 0 {
		t.Fatal("cleared session restored")
	}
}

func TestHOYTransportErrorsDoNotLogCredentials(t *testing.T) {
	var logs bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(original)
	for _, failAPI := range []bool{true, false} {
		m := newHOYResolver(func(string) (*http.Client, error) {
			return &http.Client{Transport: tvbTransport(func(r *http.Request) (*http.Response, error) {
				if !failAPI && r.URL.Hostname() == "api2.hoy.tv" {
					return hoyTestResponse(r, hoyTestData(time.Now())), nil
				}
				return nil, errors.New("private-proxy-password private-signature")
			})}, nil
		})
		p, err := m.Resolve(context.Background(), hoyTestChannel(), core.Settings{}, "config")
		if !failAPI {
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.Session.Fetch(context.Background(), p.URL, "")
		}
		if err == nil || strings.Contains(err.Error(), "private-") {
			t.Fatal("raw transport error exposed")
		}
		m.Invalidate(hoyTestChannel().ID)
	}
	if strings.Contains(logs.String(), "private-") || strings.Contains(logs.String(), "Policy=") {
		t.Fatal("credentials exposed in logs")
	}
}
