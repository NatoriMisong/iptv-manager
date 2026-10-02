package media

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"iptv-manager/internal/core"
	"iptv-manager/internal/resolver"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type truncatedBody struct{ sent bool }

func (b *truncatedBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, []byte("partial-segment")), nil
	}
	return 0, io.ErrUnexpectedEOF
}
func (*truncatedBody) Close() error { return nil }

func TestTruncatedUnknownLengthSegmentAbortsResponse(t *testing.T) {
	repo := &integrationRepo{traffic: make(map[string]int64)}
	res := &integrationResolver{results: []resolver.Result{{URL: "https://test.googlevideo.com/live", ExpiresAt: time.Now().Add(time.Hour)}}}
	s := New(repo, res, Options{CacheBytes: 1 << 20, ClientFactory: func(string) (*http.Client, error) {
		return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"video/mp2t"}}, ContentLength: -1, Body: &truncatedBody{}, Request: req}, nil
		})}, nil
	}})
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Errorf("expected abort, got %v", got)
		}
		if len(s.cache.items) != 0 {
			t.Error("truncated content cached")
		}
		if err := s.FlushTraffic(context.Background()); err != nil {
			t.Error(err)
		}
		if res.invalidations != 1 {
			t.Error("source was not invalidated")
		}
	}()
	_, err := s.serve(httptest.NewRecorder(), httptest.NewRequest("GET", "/media/id", nil), resource{URL: "https://test.googlevideo.com/segment", Channel: "one"}, core.Settings{}, false)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Error(err)
	}
}
