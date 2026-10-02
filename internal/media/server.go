package media

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"iptv-manager/internal/core"
	"iptv-manager/internal/resolver"
	"iptv-manager/internal/source"
)

type Repository interface {
	Channels(context.Context) ([]core.Channel, error)
	Channel(context.Context, string) (core.Channel, error)
	Settings(context.Context) (core.Settings, error)
	AddTraffic(context.Context, string, int64) error
}
type Resolver interface {
	Resolve(context.Context, core.Channel, core.Settings) (resolver.Result, error)
	Invalidate(string)
}
type Options struct {
	CacheBytes   int64
	MaxFetches   int
	MaxResources int
	// Injection points for local integration tests. Production uses the guarded client.
	ClientFactory       func(string) (*http.Client, error)
	ValidateURL         func(string) error
	StreamClientFactory func(string) (*http.Client, error)
	ValidateStreamURL   func(string) error
	TVBClientFactory    func(string) (*http.Client, error)
}
type resource struct {
	URL         string
	Channel     string
	Fingerprint string
	Proxy       string
	Headers     map[string]string
	Expires     time.Time
	Playlist    bool
	Refreshable bool
	Path        []string
	RootURL     string
	RootExpires time.Time
	VideoURL    string
	VideoFormat resolver.VideoFormat
	Height      int
	Direct      bool
	Stream      bool
	StreamRoot  bool
	TVB         *tvbSession
}
type Server struct {
	repo               Repository
	resolver           Resolver
	tvb                *tvbResolver
	options            Options
	cache              *segmentCache
	mu                 sync.Mutex
	resources          map[string]resource
	failures           map[string]core.ChannelStatus
	clients            map[string]*http.Client
	flights            map[string]chan struct{}
	fetches            chan struct{}
	refreshes          chan struct{}
	selectionRefreshes map[string]time.Time
	trafficMu          sync.Mutex
	pending            map[string]int64
}

func New(repo Repository, res Resolver, opts Options) *Server {
	if opts.MaxFetches <= 0 {
		opts.MaxFetches = 8
	}
	if opts.MaxResources <= 0 {
		opts.MaxResources = 4096
	}
	if opts.ClientFactory == nil {
		opts.ClientFactory = newClient
	}
	if opts.ValidateURL == nil {
		opts.ValidateURL = ValidateUpstream
	}
	if opts.StreamClientFactory == nil {
		opts.StreamClientFactory = source.NewClient
	}
	if opts.ValidateStreamURL == nil {
		opts.ValidateStreamURL = source.ValidateURL
	}
	if opts.TVBClientFactory == nil {
		opts.TVBClientFactory = newTVBClient
	}
	return &Server{repo: repo, resolver: res, tvb: newTVBResolver(opts.TVBClientFactory), options: opts, cache: newSegmentCache(opts.CacheBytes), resources: make(map[string]resource), failures: make(map[string]core.ChannelStatus), clients: make(map[string]*http.Client), flights: make(map[string]chan struct{}), fetches: make(chan struct{}, opts.MaxFetches), refreshes: make(chan struct{}, 1), selectionRefreshes: make(map[string]time.Time), pending: make(map[string]int64)}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /playlist.m3u", s.playlist)
	mux.HandleFunc("GET /watch/{id}", s.watch)
	mux.HandleFunc("GET /media/{id}", s.media)
	return mux
}
func fail(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, message, status)
}
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) (core.Settings, bool) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	settings, err := s.repo.Settings(r.Context())
	if err != nil {
		fail(w, 503, "配置暂不可用")
		return settings, false
	}
	token := r.URL.Query().Get("token")
	if token == "" || settings.PlaybackToken == "" || subtle.ConstantTimeCompare([]byte(token), []byte(settings.PlaybackToken)) != 1 {
		fail(w, 401, "播放令牌无效")
		return settings, false
	}
	return settings, true
}
func (s *Server) playlist(w http.ResponseWriter, r *http.Request) {
	settings, ok := s.authorize(w, r)
	if !ok {
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "default"
	}
	if mode != "default" && mode != "relay" && mode != "direct" {
		fail(w, 400, "播放模式无效")
		return
	}
	if settings.BaseURL == "" {
		fail(w, 503, "请先在设置中填写服务访问地址")
		return
	}
	channels, err := s.repo.Channels(r.Context())
	if err != nil {
		fail(w, 503, "频道列表暂不可用")
		return
	}
	sort.SliceStable(channels, func(i, j int) bool { return channels[i].SortOrder < channels[j].SortOrder })
	var body strings.Builder
	body.WriteString("#EXTM3U\n")
	for _, ch := range channels {
		if !ch.Enabled || ch.SourceMissing {
			continue
		}
		fmt.Fprintf(&body, "#EXTINF:-1 tvg-id=\"%s\" tvg-name=\"%s\" group-title=\"%s\" tvg-logo=\"%s\",%s\n", ch.ID, ch.Name, ch.Group, ch.Logo, ch.Name)
		q := url.Values{"token": {settings.PlaybackToken}, "mode": {mode}}
		fmt.Fprintf(&body, "%s/watch/%s?%s\n", settings.BaseURL, url.PathEscape(ch.ID), q.Encode())
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `inline; filename="iptv-manager.m3u"`)
	io.WriteString(w, body.String())
}
func fingerprint(ch core.Channel, settings core.Settings) string {
	b, _ := json.Marshal([]any{ch.URL, ch.SourceType, ch.Quality, settings.DefaultQuality, core.EffectiveProxy(ch, settings)})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func (s *Server) watch(w http.ResponseWriter, r *http.Request) {
	settings, ok := s.authorize(w, r)
	if !ok {
		return
	}
	ch, err := s.repo.Channel(r.Context(), r.PathValue("id"))
	if err != nil || !ch.Enabled || ch.SourceMissing {
		fail(w, 404, "频道不存在或已停用")
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode == "" || mode == "default" {
		mode = ch.Mode
		if mode == "" || mode == "inherit" {
			mode = settings.DefaultMode
		}
	}
	if mode != "relay" && mode != "direct" {
		fail(w, 400, "播放模式无效")
		return
	}
	if ch.IsTVB() {
		s.watchTVB(w, r, ch, settings, mode)
		return
	}
	res := resolver.Result{URL: ch.URL}
	if !ch.IsStream() {
		res, err = s.resolver.Resolve(r.Context(), ch, settings)
	}
	if err != nil {
		w.Header().Set("Retry-After", "15")
		fail(w, 503, err.Error())
		return
	}
	if err = s.validate(res.URL, ch.IsStream()); err != nil {
		fail(w, 502, "来源地址不被允许")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if mode == "direct" && res.VideoURL == "" {
		if ch.IsStream() {
			s.reportState(ch.ID, "unknown", "客户端直连，服务器未检测可播性")
		}
		http.Redirect(w, r, res.URL, http.StatusTemporaryRedirect)
		return
	}
	if mode == "relay" && settings.BaseURL == "" {
		fail(w, 503, "请先设置服务访问地址")
		return
	}
	ref := resource{URL: res.URL, Channel: ch.ID, Fingerprint: fingerprint(ch, settings), Proxy: core.EffectiveProxy(ch, settings), Headers: res.Headers, Playlist: !ch.IsStream(), Refreshable: !ch.IsStream(), RootURL: res.URL, RootExpires: res.ExpiresAt, VideoURL: res.VideoURL, VideoFormat: res.VideoFormat, Height: res.Height, Direct: mode == "direct", Stream: ch.IsStream(), StreamRoot: ch.IsStream()}
	for attempt := 0; attempt < 2; attempt++ {
		status, err := s.serve(w, r, ref, settings, !ch.IsStream())
		if err == nil {
			return
		}
		if !ch.IsStream() && attempt == 0 && (expiredStatus(status) || errors.Is(err, errMasterSelection)) {
			ref, err = s.refreshPlaylist(r.Context(), ch, settings, ref, failureRefreshReason(err))
			if err != nil {
				if errors.Is(err, errSelectionCooldown) {
					w.Header().Set("Retry-After", "30")
				}
				fail(w, 503, err.Error())
				return
			}
			continue
		}
		fail(w, 502, "直播来源暂不可用，请稍后重试")
		return
	}
}
func (s *Server) reference(ref resource, settings core.Settings) (string, error) {
	if ref.TVB != nil {
		if !s.tvb.active(ref.TVB) {
			return "", errTVBRevoked
		}
		if err := validateTVBURL(ref.URL); err != nil {
			return "", err
		}
	}
	if err := s.validate(ref.URL, ref.Stream); err != nil {
		return "", err
	}
	b, _ := json.Marshal([]any{ref.URL, ref.Channel, ref.Fingerprint, ref.Headers, ref.Path, ref.VideoURL, ref.VideoFormat, ref.Height, tvbSessionID(ref)})
	sum := sha256.Sum256(b)
	id := hex.EncodeToString(sum[:20])
	ref.Expires = time.Now().Add(2 * time.Hour)
	s.mu.Lock()
	for key, value := range s.resources {
		if time.Now().After(value.Expires) {
			delete(s.resources, key)
		}
	}
	if _, ok := s.resources[id]; !ok && len(s.resources) >= s.options.MaxResources {
		var oldest string
		var candidate resource
		for key, value := range s.resources {
			if oldest == "" || (candidate.Playlist && !value.Playlist) || (candidate.Playlist == value.Playlist && value.Expires.Before(candidate.Expires)) {
				oldest = key
				candidate = value
			}
		}
		delete(s.resources, oldest)
	}
	s.resources[id] = ref
	s.mu.Unlock()
	return settings.BaseURL + "/media/" + id + "?token=" + url.QueryEscape(settings.PlaybackToken), nil
}
func (s *Server) media(w http.ResponseWriter, r *http.Request) {
	settings, ok := s.authorize(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	ref, found := s.resources[r.PathValue("id")]
	if found && !time.Now().After(ref.Expires) {
		ref.Expires = time.Now().Add(2 * time.Hour)
		s.resources[r.PathValue("id")] = ref
	}
	s.mu.Unlock()
	if !found || time.Now().After(ref.Expires) {
		fail(w, 410, "播放资源已过期，请重新打开频道")
		return
	}
	ch, err := s.repo.Channel(r.Context(), ref.Channel)
	if err != nil || !ch.Enabled || ch.SourceMissing || ref.Fingerprint != fingerprint(ch, settings) {
		fail(w, 410, "频道配置已更新，请重新打开频道")
		return
	}
	if ref.TVB != nil {
		s.serveTVB(w, r, ch, settings, ref, r.PathValue("id"))
		return
	}
	if ref.Playlist && ref.Refreshable && !ref.RootExpires.IsZero() && !time.Now().Before(ref.RootExpires.Add(-time.Minute)) {
		ref, err = s.refreshPlaylist(r.Context(), ch, settings, ref, refreshExpiry)
		if err != nil {
			fail(w, 502, "直播清单刷新失败，请稍后重试")
			return
		}
		s.saveReference(r.PathValue("id"), ref)
	}
	status, err := s.serve(w, r, ref, settings, false)
	if err != nil && (expiredStatus(status) || errors.Is(err, errMasterSelection)) && ref.Playlist && ref.Refreshable {
		ref, err = s.refreshPlaylist(r.Context(), ch, settings, ref, failureRefreshReason(err))
		if err == nil {
			s.saveReference(r.PathValue("id"), ref)
			status, err = s.serve(w, r, ref, settings, false)
		}
	}
	if err != nil {
		if expiredStatus(status) && !ref.Playlist && !ref.Stream {
			s.resolver.Invalidate(ch.ID)
		}
		if errors.Is(err, errSelectionCooldown) {
			status = 503
			w.Header().Set("Retry-After", "30")
		} else if status == 503 {
			w.Header().Set("Retry-After", "2")
		} else if status != 416 {
			status = 502
		}
		fail(w, status, "媒体资源暂不可用，请重新打开频道")
	}
}
func (s *Server) validate(raw string, stream bool) error {
	if stream {
		return s.options.ValidateStreamURL(raw)
	}
	return s.options.ValidateURL(raw)
}
func (s *Server) client(proxy string, stream bool) (*http.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fmt.Sprintf("%t:%s", stream, proxy)
	if c, ok := s.clients[key]; ok {
		return c, nil
	}
	factory := s.options.ClientFactory
	if stream {
		factory = s.options.StreamClientFactory
	}
	c, err := factory(proxy)
	if err != nil {
		return nil, err
	}
	if len(s.clients) >= 16 {
		for k, v := range s.clients {
			v.CloseIdleConnections()
			delete(s.clients, k)
		}
	}
	s.clients[key] = c
	return c, nil
}
func (s *Server) fetch(ctx context.Context, ref resource, rangeHeader string) (*http.Response, error) {
	if ref.TVB != nil {
		return s.fetchTVB(ctx, ref, rangeHeader)
	}
	if err := s.validate(ref.URL, ref.Stream); err != nil {
		return nil, err
	}
	c, err := s.client(ref.Proxy, ref.Stream)
	if err != nil {
		return nil, err
	}
	var cancel context.CancelFunc
	if ref.StreamRoot {
		ctx, cancel = context.WithCancel(ctx)
		copy := *c
		copy.Timeout = 0
		c = &copy
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.URL, nil)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, err
	}
	for k, v := range ref.Headers {
		switch http.CanonicalHeaderKey(k) {
		case "User-Agent", "Referer", "Origin", "Accept", "Accept-Language":
			req.Header.Set(k, v)
		}
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, err := c.Do(req)
	if cancel != nil {
		if err != nil {
			cancel()
		} else {
			resp.Body = &idleBody{ReadCloser: resp.Body, cancel: cancel, timer: time.AfterFunc(35*time.Second, cancel)}
		}
	}
	if err != nil {
		reason := "network_error"
		var networkErr net.Error
		if errors.As(err, &networkErr) && networkErr.Timeout() {
			reason = "timeout"
		}
		if errors.Is(err, context.Canceled) {
			reason = "cancelled"
		}
		// Do not log URLs or raw transport errors: signed addresses and proxy
		// credentials can otherwise leak through standard net/url errors.
		slog.Warn("媒体上游请求失败", "channel", ref.Channel, "host", req.URL.Hostname(), "reason", reason)
		if reason != "cancelled" {
			s.reportFailure(ref.Channel, "媒体请求失败，请检查服务器出口和网络（"+reason+"）")
		}
	}
	return resp, err
}

// Continuous HTTP streams have no total lifetime limit; a stalled read is bounded.
type idleBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	timer  *time.Timer
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(35 * time.Second)
	}
	return n, err
}
func (b *idleBody) Close() error { b.timer.Stop(); b.cancel(); return b.ReadCloser.Close() }
func (s *Server) begin(key string) (chan struct{}, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if wait, ok := s.flights[key]; ok {
		return wait, false
	}
	if len(s.flights) >= 64 {
		return nil, false
	}
	wait := make(chan struct{})
	s.flights[key] = wait
	return wait, true
}
func (s *Server) end(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.flights[key]; ok {
		delete(s.flights, key)
		close(c)
	}
}
func segmentHeaders(h http.Header) http.Header {
	copy := make(http.Header)
	for _, key := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := h.Get(key); v != "" {
			copy.Set(key, v)
		}
	}
	return copy
}
func (s *Server) serve(w http.ResponseWriter, r *http.Request, ref resource, settings core.Settings, requireManifest bool) (int, error) {
	if ref.TVB != nil && !s.tvb.active(ref.TVB) {
		return 410, errTVBRevoked
	}
	key := ref.Fingerprint + "\x00" + ref.Channel + "\x00" + ref.URL + "\x00" + r.Header.Get("Range") + "\x00" + tvbSessionID(ref)
	if !requireManifest && !ref.Playlist && !ref.StreamRoot {
		for {
			if cached, ok := s.cache.get(key); ok {
				for k, v := range cached.header {
					w.Header()[k] = v
				}
				w.Header().Set("Cache-Control", "private, max-age=10")
				w.WriteHeader(cached.status)
				if r.Method != "HEAD" {
					n, _ := w.Write(cached.data)
					s.count(int64(n))
				}
				return cached.status, nil
			}
			wait, leader := s.begin(key)
			if leader {
				defer s.end(key)
				break
			}
			if wait == nil {
				return 503, errors.New("media queue full")
			}
			select {
			case <-wait:
			case <-r.Context().Done():
				return 0, r.Context().Err()
			}
		}
	}
	select {
	case s.fetches <- struct{}{}:
		defer func() { <-s.fetches }()
	case <-r.Context().Done():
		return 0, r.Context().Err()
	}
	resp, err := s.fetch(r.Context(), ref, r.Header.Get("Range"))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		slog.Warn("媒体上游返回错误", "channel", ref.Channel, "status", resp.StatusCode)
		s.reportFailure(ref.Channel, fmt.Sprintf("媒体来源返回 HTTP %d，请刷新来源或检查出口", resp.StatusCode))
		return resp.StatusCode, errors.New("upstream failure")
	}
	reader := bufio.NewReaderSize(resp.Body, 4096)
	prefix, _ := reader.Peek(10)
	prefix = bytes.TrimSpace(bytes.TrimPrefix(prefix, []byte("\ufeff")))
	isManifest := requireManifest || bytes.HasPrefix(prefix, []byte("#EXTM3U")) || strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "mpegurl")
	if isManifest {
		body, err := io.ReadAll(io.LimitReader(reader, 2*1024*1024+1))
		if err != nil {
			return 502, err
		}
		if len(body) > 2*1024*1024 {
			return 502, errors.New("manifest too large")
		}
		base := resp.Request.URL
		body, err = s.prepareManifest(body, base, ref)
		if err != nil {
			return 502, err
		}
		body, err = rewriteHLSLinks(body, base, func(link playlistLink) (string, error) {
			if ref.Direct {
				if err := s.validate(link.URL, ref.Stream); err != nil {
					return "", err
				}
				return link.URL, nil
			}
			next := ref
			next.URL = link.URL
			next.StreamRoot = false
			next.Playlist = link.Playlist
			next.Refreshable = ref.Refreshable && link.Playlist && link.Selector != "" && len(ref.Path) < 8
			if next.Refreshable {
				next.Path = append(append([]string(nil), ref.Path...), link.Selector)
			} else {
				next.Path = nil
			}
			return s.reference(next, settings)
		})
		if err != nil {
			if ref.Stream {
				s.reportFailure(ref.Channel, "清单包含无效或不允许的资源地址")
			}
			return 502, err
		}
		if ref.Stream {
			s.reportState(ref.Channel, "ready", "直播清单可用，实际播放由播放器确认")
		} else {
			s.clearFailure(ref.Channel)
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method != "HEAD" {
			n, _ := w.Write(body)
			s.count(int64(n))
		}
		return 200, nil
	}
	if ref.StreamRoot && (strings.HasPrefix(resp.Header.Get("Content-Type"), "text/") || strings.Contains(resp.Header.Get("Content-Type"), "json")) {
		s.reportFailure(ref.Channel, "来源返回了网页或文本，未发现直播媒体")
		return 502, errors.New("not a media response")
	}
	h := segmentHeaders(resp.Header)
	if ref.Stream {
		kind := strings.ToLower(strings.Split(h.Get("Content-Type"), ";")[0])
		if !strings.HasPrefix(kind, "video/") && !strings.HasPrefix(kind, "audio/") && kind != "application/mp4" {
			h.Set("Content-Type", "application/octet-stream")
		}
	}
	for k, v := range h {
		w.Header()[k] = v
	}
	w.Header().Set("Cache-Control", "private, max-age=10")
	w.WriteHeader(resp.StatusCode)
	if r.Method == "HEAD" {
		return resp.StatusCode, nil
	}
	// At most 4 MiB are captured per in-flight request; larger segments continue
	// streaming without being cached. The completed cache has a separate cap.
	var capture bytes.Buffer
	cacheable := !ref.StreamRoot && s.options.CacheBytes > 0 && (resp.ContentLength < 0 || resp.ContentLength <= 4*1024*1024)
	buffer := make([]byte, 32*1024)
	var total int64
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			if ref.StreamRoot {
				_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(35 * time.Second))
			}
			written, writeErr := w.Write(buffer[:n])
			total += int64(written)
			if writeErr != nil || written != n {
				s.count(total)
				return resp.StatusCode, nil
			}
			if ref.StreamRoot {
				if total == int64(written) {
					s.reportState(ref.Channel, "ready", "正在中继原始媒体")
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			if cacheable {
				if capture.Len()+n <= 4*1024*1024 {
					capture.Write(buffer[:n])
				} else {
					cacheable = false
					capture.Reset()
				}
			}
		}
		if readErr != nil {
			s.count(total)
			if r.Context().Err() != nil {
				return resp.StatusCode, nil
			}
			if readErr != io.EOF || (resp.ContentLength >= 0 && total != resp.ContentLength) {
				// A normal return would terminate a chunked response successfully even
				// though the upstream segment was truncated. Abort the response instead.
				if !ref.Stream {
					s.resolver.Invalidate(ref.Channel)
				}
				s.reportFailure(ref.Channel, "视频分片传输中断，请检查网络或重新打开频道")
				panic(http.ErrAbortHandler)
			}
			if ref.Stream {
				s.reportState(ref.Channel, "ready", "媒体传输正常")
			} else {
				s.clearFailure(ref.Channel)
			}
			if readErr == io.EOF && cacheable && (resp.ContentLength < 0 || total == resp.ContentLength) {
				s.cache.put(cachedSegment{key: key, data: capture.Bytes(), header: h, status: resp.StatusCode})
			}
			// Headers may already have been sent. Never append an error page to media.
			return resp.StatusCode, nil
		}
	}
}
func (s *Server) count(n int64) {
	s.trafficMu.Lock()
	s.pending[time.Now().UTC().Format("2006-01")] += n
	s.trafficMu.Unlock()
}
func (s *Server) FlushTraffic(ctx context.Context) error {
	s.trafficMu.Lock()
	batch := s.pending
	s.pending = make(map[string]int64)
	s.trafficMu.Unlock()
	var first error
	for month, n := range batch {
		if n == 0 {
			continue
		}
		if err := s.repo.AddTraffic(ctx, month, n); err != nil {
			s.trafficMu.Lock()
			s.pending[month] += n
			s.trafficMu.Unlock()
			if first == nil {
				first = err
			}
		}
	}
	return first
}
