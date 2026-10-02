package media

import (
	"context"
	"errors"
	"net/http"

	"iptv-manager/internal/core"
	"iptv-manager/internal/provider"
)

func (s *Server) watchSession(w http.ResponseWriter, r *http.Request, ch core.Channel, settings core.Settings, mode string, e provider.Session) {
	if mode == "direct" {
		s.reportState(ch.ID, "unknown", "来源地址已解析；直连由播放器维护来源会话")
		http.Redirect(w, r, e.URL(), http.StatusTemporaryRedirect)
		return
	}
	if settings.BaseURL == "" {
		fail(w, 503, "请先设置服务访问地址")
		return
	}
	ref := resource{URL: e.URL(), RootURL: e.URL(), Channel: ch.ID, Fingerprint: fingerprint(ch, settings), Proxy: core.EffectiveProxy(ch, settings), Playlist: true, Refreshable: true, Stream: true, Session: e}
	s.serveSession(w, r, ch, settings, ref, "")
}

func (s *Server) refreshSession(ctx context.Context, ch core.Channel, settings core.Settings, ref resource) (resource, error) {
	playback, err := s.providers.Resolve(ctx, ch, settings, fingerprint(ch, settings))
	if err != nil {
		return ref, err
	}
	e := playback.Session
	if e == nil {
		return ref, errors.New("来源未返回播放会话")
	}
	updated := ref
	updated.Session, updated.URL, updated.RootURL = e, e.URL(), e.URL()
	for _, selector := range ref.Path {
		body, base, err := s.readManifest(ctx, updated)
		if err != nil {
			return ref, err
		}
		updated.URL, err = findPlaylist(body, base, selector)
		if err != nil {
			return ref, err
		}
	}
	return updated, nil
}

func (s *Server) serveSession(w http.ResponseWriter, r *http.Request, ch core.Channel, settings core.Settings, ref resource, referenceID string) {
	if !ref.Session.CanRefresh() {
		fail(w, 410, provider.ErrRevoked.Error())
		return
	}
	if !ref.Session.Active() || ref.Session.Expired() {
		if !ref.Playlist || !ref.Refreshable {
			fail(w, 410, "来源会话已过期，请重新读取直播清单")
			return
		}
		updated, err := s.refreshSession(r.Context(), ch, settings, ref)
		if err != nil {
			s.reportFailure(ch.ID, err.Error())
			fail(w, 502, "来源刷新失败，请稍后重试或刷新来源")
			return
		}
		ref = updated
	}
	for attempt := 0; attempt < 2; attempt++ {
		if referenceID != "" {
			s.saveReference(referenceID, ref)
		}
		status, err := s.serve(w, r, ref, settings, ref.Playlist)
		if err == nil {
			return
		}
		if (status == 401 || status == 403) && ref.Session.Active() {
			ref.Session.Reject()
			// Only a fresh playlist can safely renew the semantic path. Never
			// substitute an old encrypted segment/key using a new session.
			if attempt == 0 && ref.Playlist && ref.Refreshable {
				updated, refreshErr := s.refreshSession(r.Context(), ch, settings, ref)
				if refreshErr == nil {
					ref = updated
					continue
				}
			}
		}
		if errors.Is(err, provider.ErrRevoked) {
			fail(w, 410, err.Error())
			return
		}
		s.reportFailure(ch.ID, "来源媒体暂不可用，请重新打开频道或刷新来源")
		fail(w, 502, "来源媒体暂不可用，请重新打开频道或刷新来源")
		return
	}
}

func sessionID(ref resource) string {
	if ref.Session == nil {
		return ""
	}
	return ref.Session.ID()
}
