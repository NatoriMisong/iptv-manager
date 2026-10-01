package media

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"youtube-tv/internal/core"
)

func expiredStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusGone
}

func (s *Server) saveReference(id string, ref resource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Another request may have evicted this reference while a slow refresh ran.
	// Do not bypass the global resource cap by recreating it here.
	if _, ok := s.resources[id]; ok {
		ref.Expires = time.Now().Add(2 * time.Hour)
		s.resources[id] = ref
	}
}

// refreshPlaylist replays only the semantic path through master playlists.
// It never maps an old media segment to a new segment by position or filename.
// This preserves an audio rendition or a selected resolution when the provider
// renews signatures or reorders entries in a master playlist.
func (s *Server) refreshPlaylist(ctx context.Context, ch core.Channel, settings core.Settings, ref resource, force bool) (resource, error) {
	if !ref.Playlist || !ref.Refreshable || len(ref.Path) > 8 {
		return ref, errors.New("此资源不能自动刷新")
	}
	select {
	case s.refreshes <- struct{}{}:
		defer func() { <-s.refreshes }()
	case <-ctx.Done():
		return ref, ctx.Err()
	}
	resolved, err := s.resolver.Resolve(ctx, ch, settings)
	if err != nil {
		return ref, err
	}
	// Recheck after acquiring the gate. A different viewer may have already
	// refreshed this root; invalidating again would cancel their shared work.
	if force && resolved.URL == ref.RootURL {
		s.resolver.Invalidate(ch.ID)
		resolved, err = s.resolver.Resolve(ctx, ch, settings)
		if err != nil {
			return ref, err
		}
	}
	if err := s.options.ValidateURL(resolved.URL); err != nil {
		return ref, err
	}
	updated := ref
	updated.URL = resolved.URL
	updated.RootURL = resolved.URL
	updated.RootExpires = resolved.ExpiresAt
	updated.Headers = resolved.Headers
	updated.Proxy = core.EffectiveProxy(ch, settings)
	for _, selector := range ref.Path {
		body, base, err := s.readManifest(ctx, updated)
		if err != nil {
			return ref, err
		}
		next, err := findPlaylist(body, base, selector)
		if err != nil {
			return ref, err
		}
		updated.URL = next
	}
	return updated, nil
}

func (s *Server) readManifest(ctx context.Context, ref resource) ([]byte, *url.URL, error) {
	select {
	case s.fetches <- struct{}{}:
		defer func() { <-s.fetches }()
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	resp, err := s.fetch(ctx, ref, "")
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, errors.New("刷新后的上游清单不可用")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	if err != nil {
		return nil, nil, err
	}
	if len(body) > 2*1024*1024 {
		return nil, nil, errors.New("HLS 清单过大")
	}
	return body, resp.Request.URL, nil
}

func findPlaylist(body []byte, base *url.URL, selector string) (string, error) {
	var found string
	_, err := rewriteHLSLinks(body, base, func(link playlistLink) (string, error) {
		if link.Playlist && link.Selector == selector {
			if found != "" && found != link.URL {
				return "", errors.New("新清单包含多个匹配的直播流，无法安全刷新")
			}
			found = link.URL
		}
		return link.URL, nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", errors.New("新清单中找不到原来的直播流，请重新打开频道")
	}
	return found, nil
}
