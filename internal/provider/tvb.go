package provider

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"iptv-manager/internal/core"
	"iptv-manager/internal/source"
)

// TVB session state is deliberately kept out of SQLite and configuration
// backups. Each channel/configuration has its own client and cookie jar.
type tvbSession struct {
	owner        *tvbResolver
	id           uint64
	family       uint64
	retryAt      time.Time
	channel      string
	config       string
	code         string
	ready        chan struct{}
	ctx          context.Context
	cancel       context.CancelFunc
	client       *http.Client
	jar          *tvbCookieJar
	root         string
	until        time.Time
	err          error
	lastUsed     time.Time // protected by tvbResolver.mu
	stale        atomic.Bool
	cookieLogged atomic.Bool
}

type tvbResolver struct {
	mu       sync.Mutex
	sessions map[string]*tvbSession
	serial   uint64
	fetches  chan struct{}
	client   func(string) (*http.Client, error)
	now      func() time.Time
}

func newTVBResolver(factory func(string) (*http.Client, error)) *tvbResolver {
	if factory == nil {
		factory = newTVBClient
	}
	return &tvbResolver{sessions: make(map[string]*tvbSession), fetches: make(chan struct{}, 2), client: factory, now: time.Now}
}

// A configured proxy resolves TVB's allowed hosts, including its AES key host.
// Direct connections retain the actual dial-IP guard. Generic URL sources
// continue to use their separate, more general destination checks.
func newTVBClient(proxy string) (*http.Client, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{ForceAttemptHTTP2: true, MaxIdleConns: 16, MaxIdleConnsPerHost: 8, IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second, MaxResponseHeaderBytes: 64 * 1024, DisableCompression: true}
	if proxy != "" {
		p, err := url.Parse(proxy)
		if err != nil {
			return nil, errors.New("代理地址无效")
		}
		transport.Proxy = http.ProxyURL(p)
	} else {
		dialer.ControlContext = func(_ context.Context, _, addr string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return err
			}
			if !source.PublicAddress(net.ParseIP(host)) {
				return errors.New("上游解析到了非公网地址")
			}
			return nil
		}
	}
	transport.DialContext = dialer.DialContext
	client := &http.Client{Transport: transport, Timeout: 35 * time.Second}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("TVB 重定向过多")
		}
		return validateTVBURL(req.URL.String())
	}
	return client, nil
}

// Cookie operations and revocation share a lock: a late upstream response
// cannot restore a cookie after the user has clicked "refresh source".
type tvbCookieJar struct {
	mu      sync.Mutex
	jar     http.CookieJar
	hadAuth bool
}

func newTVBCookieJar() *tvbCookieJar {
	jar, _ := cookiejar.New(nil)
	return &tvbCookieJar{jar: jar}
}

func (j *tvbCookieJar) Cookies(u *url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.jar == nil || validateTVBURL(u.String()) != nil {
		return nil
	}
	return j.jar.Cookies(u)
}

func (j *tvbCookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.jar == nil || validateTVBURL(u.String()) != nil {
		return
	}
	var accepted []*http.Cookie
	for _, cookie := range cookies {
		domain := strings.ToLower(strings.TrimPrefix(cookie.Domain, "."))
		// All requests are confined to TVB. Reject supercookies such as .com
		// without adding a public-suffix dependency solely for these hosts.
		if domain != "" && domain != "tvb.com" && !strings.HasSuffix(domain, ".tvb.com") {
			continue
		}
		if len(cookie.Name)+len(cookie.Value) > 4096 || len(accepted) >= 32 {
			continue
		}
		if cookie.Name == "hdntl" {
			j.hadAuth = true
		}
		accepted = append(accepted, cookie)
	}
	j.jar.SetCookies(u, accepted)
}

func (j *tvbCookieJar) expired(raw string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.jar == nil {
		return true
	}
	if !j.hadAuth {
		return false
	}
	u, _ := url.Parse(raw)
	for _, cookie := range j.jar.Cookies(u) {
		if cookie.Name == "hdntl" && cookie.Value != "" {
			return false
		}
	}
	return true
}

func (j *tvbCookieJar) clear() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.jar = nil
}

func validateTVBURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") || len(raw) > 16384 {
		return errors.New("TVB 返回的资源地址无效")
	}
	host := strings.ToLower(u.Hostname())
	if host != "tvb.com" && !strings.HasSuffix(host, ".tvb.com") {
		return errors.New("TVB 资源必须属于 TVB 媒体域名")
	}
	return nil
}

func (e *tvbSession) expired(now time.Time) bool {
	return e.stale.Load() || !now.Before(e.until) || (e.err == nil && e.jar.expired(e.root))
}

func (m *tvbResolver) revoke(e *tvbSession) {
	e.cancel()
	e.jar.clear()
	select {
	case <-e.ready:
		if e.client != nil {
			e.client.CloseIdleConnections()
		}
	default:
	}
}

func (m *tvbResolver) invalidate(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.sessions[id]; e != nil {
		m.revoke(e)
		delete(m.sessions, id)
		slog.Info("TVB 来源与 Cookie 缓存已清除", "channel", id, "next_action", "下次播放按需获取")
	}
}

func (m *tvbResolver) active(e *tvbSession) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[e.channel] != e || e.ctx.Err() != nil {
		return false
	}
	e.lastUsed = m.now()
	return true
}

func (m *tvbResolver) canRefresh(e *tvbSession) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.sessions[e.channel]
	return current != nil && current.family == e.family && current.ctx.Err() == nil
}

// Resolve joins one in-flight request per channel. No timer or background
// refresh runs; only playback requests start a bounded API request.
func (m *tvbResolver) resolve(ctx context.Context, ch core.Channel, settings core.Settings, config string) (*tvbSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	code := tvbChannelID(ch.URL)
	if code == "" {
		return nil, errors.New("TVB 仅支持新闻 C 和财经 F 频道")
	}
	m.mu.Lock()
	e := m.sessions[ch.ID]
	var family uint64
	if e != nil {
		ready := false
		select {
		case <-e.ready:
			ready = true
		default:
		}
		if e.config == config && ready && e.stale.Load() && m.now().Before(e.retryAt) {
			m.mu.Unlock()
			return nil, errors.New("TVB 上游拒绝了更新后的会话，请稍后重试或手动刷新来源")
		}
		if e.config != config || (ready && e.expired(m.now())) {
			if e.config == config {
				family = e.family
			}
			m.revoke(e)
			delete(m.sessions, ch.ID)
			e = nil
		}
	}
	if e == nil {
		if len(m.sessions) >= 64 {
			var oldest *tvbSession
			for _, candidate := range m.sessions {
				select {
				case <-candidate.ready:
					if oldest == nil || candidate.lastUsed.Before(oldest.lastUsed) {
						oldest = candidate
					}
				default:
				}
			}
			if oldest == nil {
				m.mu.Unlock()
				return nil, errors.New("TVB 解析队列已满，请稍后重试")
			}
			m.revoke(oldest)
			delete(m.sessions, oldest.channel)
		}
		m.serial++
		lifetime, cancel := context.WithCancel(context.Background())
		e = &tvbSession{owner: m, id: m.serial, channel: ch.ID, config: config, code: code, ready: make(chan struct{}), ctx: lifetime, cancel: cancel, jar: newTVBCookieJar(), lastUsed: m.now()}
		e.family = family
		if family == 0 {
			e.family = e.id
		} else {
			e.retryAt = m.now().Add(15 * time.Second)
		}
		m.sessions[ch.ID] = e
		go m.initialize(e, core.EffectiveProxy(ch, settings))
	}
	e.lastUsed = m.now()
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.ready:
		if !m.active(e) {
			return nil, ErrRevoked
		}
		return e, e.err
	}
}

func (m *tvbResolver) initialize(e *tvbSession, proxy string) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(e.ctx, 40*time.Second)
	defer cancel()
	mode := "direct"
	if proxy != "" {
		mode = "proxy"
	}
	slog.Info("TVB 来源解析开始", "channel", e.channel, "tvb_channel", e.code, "proxy_mode", mode)
	var err error
	select {
	case m.fetches <- struct{}{}:
		err = m.extract(ctx, e, proxy)
		<-m.fetches
	case <-ctx.Done():
		err = errors.New("TVB 来源请求已取消或超时")
	}
	if e.ctx.Err() != nil {
		err = errors.New("TVB 来源请求已取消")
	}
	if err != nil {
		e.until = m.now().Add(15 * time.Second)
		e.jar.clear()
		slog.Warn("TVB 来源解析失败", "channel", e.channel, "duration_ms", time.Since(started).Milliseconds(), "error", err.Error())
	} else {
		slog.Info("TVB 来源解析成功", "channel", e.channel, "tvb_channel", e.code, "duration_ms", time.Since(started).Milliseconds(), "expires_at", e.until)
	}
	e.err = err
	if e.ctx.Err() != nil && e.client != nil {
		e.client.CloseIdleConnections()
	}
	close(e.ready)
}

func tvbHeaders(req *http.Request, code string) {
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://news.tvb.com/tc/live/"+code)
	req.Header.Set("Origin", "https://news.tvb.com")
}

func (m *tvbResolver) extract(ctx context.Context, e *tvbSession, proxy string) error {
	base, err := m.client(proxy)
	if err != nil {
		return errors.New("无法创建 TVB 连接，请检查代理配置")
	}
	client := *base
	client.Jar = e.jar
	client.Timeout = 30 * time.Second
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("TVB 重定向过多")
		}
		if err := validateTVBURL(req.URL.String()); err != nil {
			return err
		}
		if base.CheckRedirect != nil {
			return base.CheckRedirect(req, via)
		}
		return nil
	}
	e.client = &client
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://news.tvb.com/app/public/live/stream/"+e.code, nil)
	tvbHeaders(req, e.code)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("TVB 接口请求失败（%s），请检查出口、代理或网络", tvbNetworkReason(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("TVB 接口返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
	if err != nil || len(body) > 1024*1024 {
		return errors.New("TVB 接口响应不完整或过大")
	}
	var result struct {
		Success bool `json:"success"`
		Data    struct {
			Channel    string `json:"channel_id"`
			URL        string `json:"stream_url"`
			Protocol   string `json:"protocol"`
			GeoBlocked bool   `json:"geo_blocked"`
			Expire     int64  `json:"expire_time"`
			Interval   int64  `json:"refresh_interval"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &result) != nil || !result.Success || result.Data.Channel != e.code {
		return errors.New("TVB 未返回有效的频道播放信息")
	}
	if result.Data.GeoBlocked {
		return errors.New("TVB 当前来源在此出口地区不可用")
	}
	if result.Data.Protocol != "hls" {
		return errors.New("TVB 此频道未提供 HLS 直播来源")
	}
	if err := validateTVBURL(result.Data.URL); err != nil {
		return err
	}
	now := m.now()
	e.until = now.Add(time.Hour)
	if result.Data.Interval > 0 && result.Data.Interval < 3600 {
		e.until = now.Add(time.Duration(result.Data.Interval) * time.Second)
	}
	if result.Data.Expire > 0 && time.Unix(result.Data.Expire, 0).Before(e.until) {
		e.until = time.Unix(result.Data.Expire, 0)
	}
	if !e.until.After(now) {
		return errors.New("TVB 接口返回的来源已过期")
	}
	e.root = result.Data.URL
	// header_json is upstream response metadata, not request headers. Never
	// replay it, arbitrary authentication headers, or cookies from the API body.
	return nil
}

type tvbResponseBody struct {
	io.ReadCloser
	stop   func() bool
	cancel context.CancelFunc
}

func (b *tvbResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.stop()
	b.cancel()
	return err
}

func (e *tvbSession) Fetch(ctx context.Context, raw string, rangeHeader string) (*http.Response, error) {
	if !e.Active() {
		return nil, ErrRevoked
	}
	if err := validateTVBURL(raw); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	stop := context.AfterFunc(e.ctx, cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		stop()
		cancel()
		return nil, errors.New("TVB 资源请求无效")
	}
	tvbHeaders(req, e.code)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, err := tvbMediaRequest(e.client, req, e.channel)
	if err != nil {
		stop()
		cancel()
		if !e.Active() {
			return nil, ErrRevoked
		}
		slog.Warn("TVB 媒体请求失败", "channel", e.channel, "host", req.URL.Hostname(), "reason", tvbNetworkReason(err))
		return nil, errors.New("TVB 媒体请求失败，请检查出口和网络")
	}
	resp.Body = &tvbResponseBody{ReadCloser: resp.Body, stop: stop, cancel: cancel}
	for _, cookie := range e.jar.Cookies(req.URL) {
		if cookie.Name == "hdntl" && e.cookieLogged.CompareAndSwap(false, true) {
			slog.Info("TVB Cookie 会话已建立", "channel", e.channel, "tvb_channel", e.code)
		}
	}
	return resp, nil
}

// The TVB key host sometimes rejects the TLS handshake before HTTP starts.
// Retry only that observed failure, with a bounded GET and unchanged TLS
// verification. HTTP authorization errors are handled by the session logic.
func tvbMediaRequest(client *http.Client, req *http.Request, channel string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := client.Do(req)
		if err == nil || tvbNetworkReason(err) != "tls_handshake_failure" || attempt >= 2 || req.Context().Err() != nil {
			return resp, err
		}
		slog.Warn("TVB TLS 握手失败，准备重试", "channel", channel, "host", req.URL.Hostname(), "retry", attempt+1)
		timer := time.NewTimer(time.Duration(attempt+1) * 250 * time.Millisecond)
		select {
		case <-req.Context().Done():
			timer.Stop()
			return nil, req.Context().Err()
		case <-timer.C:
		}
	}
}

func tvbNetworkReason(err error) string {
	var remote *net.OpError
	if errors.As(err, &remote) && remote.Op == "remote error" && remote.Err.Error() == "tls: handshake failure" {
		return "tls_handshake_failure"
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "timeout"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns_error"
	}
	var certificate *tls.CertificateVerificationError
	if errors.As(err, &certificate) {
		return "tls_certificate_error"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "connection_closed"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Sprintf("transport_%T", urlErr.Err)
	}
	return "network_error"
}

func (m *tvbResolver) Resolve(ctx context.Context, ch core.Channel, settings core.Settings, config string) (Playback, error) {
	e, err := m.resolve(ctx, ch, settings, config)
	if err != nil {
		return Playback{}, err
	}
	return Playback{URL: e.root, Session: e}, nil
}
func (m *tvbResolver) Invalidate(id string)        { m.invalidate(id) }
func (e *tvbSession) ID() string                   { return strconv.FormatUint(e.id, 10) }
func (e *tvbSession) URL() string                  { return e.root }
func (e *tvbSession) Active() bool                 { return e.owner.active(e) }
func (e *tvbSession) CanRefresh() bool             { return e.owner.canRefresh(e) }
func (e *tvbSession) Expired() bool                { return e.expired(e.owner.now()) }
func (e *tvbSession) Reject()                      { e.stale.Store(true) }
func (e *tvbSession) ValidateURL(raw string) error { return validateTVBURL(raw) }

// tvbChannelID accepts only the two supported public TVB news pages.
func tvbChannelID(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "news.tvb.com") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return ""
	}
	parts := strings.Split(strings.TrimSuffix(u.Path, "/"), "/")
	if len(parts) != 4 || (parts[1] != "tc" && parts[1] != "sc" && parts[1] != "en") || parts[2] != "live" || (parts[3] != "C" && parts[3] != "F") {
		return ""
	}
	return parts[3]
}

func tvbDefinition() definition {
	return definition{catalog: Catalog{
		ID:           "tvb",
		Name:         "无线新闻",
		Website:      "https://news.tvb.com/tc/live/C",
		Description:  "无线新闻、无线财经。播放时按需获取来源和 Cookie，过期后由下次播放请求更新；刷新来源可清除会话缓存。推荐服务器中继。",
		SourceType:   "builtin",
		DefaultMode:  "relay",
		LinkLabel:    "官网直播 ↗",
		PlaybackHelp: "推荐中继；直连需要播放器维护 Cookie，并且客户端能访问来源。",
		Channels: []Channel{
			{ID: "C", Name: "无线新闻", URL: "https://news.tvb.com/tc/live/C", Selected: true, Note: "TVB 新闻 C 频道"},
			{ID: "F", Name: "无线财经", URL: "https://news.tvb.com/tc/live/F", Selected: true, Note: "TVB 财经 F 频道"},
		},
	},
		matchLegacy: func(kind, raw string) string {
			if kind == "tvb" {
				return tvbChannelID(raw)
			}
			return ""
		},
		create: func(opts Options) resolver {
			m := newTVBResolver(opts.ClientFactory)
			if opts.Now != nil {
				m.now = opts.Now
			}
			return m
		},
	}
}
