// Package provider contains broadcaster-specific catalogs, resolution and sessions.
// HLS rewriting, authorization, relay caching and traffic accounting stay in media.
package provider

import (
	"context"
	"errors"
	"net/http"
	"time"

	"iptv-manager/internal/core"
)

var ErrRevoked = errors.New("来源会话已清除，请重新打开频道")

type Channel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Selected bool   `json:"selected"`
	Note     string `json:"note"`
}

type Catalog struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	SourceType   string    `json:"source_type"`
	DefaultMode  string    `json:"default_mode"`
	Website      string    `json:"website"`
	Description  string    `json:"description"`
	LinkLabel    string    `json:"link_label"`
	PlaybackHelp string    `json:"playback_help"`
	Channels     []Channel `json:"channels"`
}

// Session owns provider authentication. Media only handles opaque session IDs
// and semantic playlist paths; it never reads Cookies or provider internals.
type Session interface {
	ID() string
	URL() string
	Active() bool
	CanRefresh() bool
	Expired() bool
	Reject()
	ValidateURL(string) error
	Fetch(context.Context, string, string) (*http.Response, error)
}

type Playback struct {
	URL     string
	Headers map[string]string
	Session Session // nil for fixed public streams
}

type Options struct {
	ClientFactory func(string) (*http.Client, error)
	Now           func() time.Time
}

type resolver interface {
	Resolve(context.Context, core.Channel, core.Settings, string) (Playback, error)
	Invalidate(string)
}

type definition struct {
	catalog     Catalog
	matchLegacy func(kind, raw string) string
	create      func(Options) resolver
}

// fixedSource shares the ordinary media transport; each broadcaster supplies
// only its address and request rules. It allocates no sessions or timers.
type fixedSource struct{ headers map[string]string }

func (p fixedSource) Resolve(_ context.Context, ch core.Channel, _ core.Settings, _ string) (Playback, error) {
	headers := make(map[string]string, len(p.headers))
	for k, v := range p.headers {
		headers[k] = v
	}
	return Playback{URL: ch.URL, Headers: headers}, nil
}
func (fixedSource) Invalidate(string) {}
