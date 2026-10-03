package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"iptv-manager/internal/core"
)

// Each saved channel/configuration owns an immutable signature and a revocable
// client session. Nothing is persisted in SQLite or configuration backups.
type hoySession struct {
	owner    *hoyResolver
	id       uint64
	family   uint64
	retryAt  time.Time
	channel  string
	config   string
	code     string
	ready    chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	client   *http.Client
	signed   hoySignature
	root     string
	until    time.Time
	err      error
	lastUsed time.Time // protected by hoyResolver.mu
	stale    atomic.Bool
}

type hoyResolver struct {
	mu       sync.Mutex
	sessions map[string]*hoySession
	serial   uint64
	fetches  chan struct{}
	client   func(string) (*http.Client, error)
	now      func() time.Time
}

func newHOYResolver(factory func(string) (*http.Client, error)) *hoyResolver {
	if factory == nil {
		factory = newHOYClient
	}
	return &hoyResolver{sessions: make(map[string]*hoySession), fetches: make(chan struct{}, 2), client: factory, now: time.Now}
}

func (e *hoySession) expired(now time.Time) bool {
	return e.stale.Load() || !now.Before(e.until)
}

func (m *hoyResolver) revoke(e *hoySession) {
	e.cancel()
	select {
	case <-e.ready:
		if e.client != nil {
			e.client.CloseIdleConnections()
		}
	default:
	}
}

func (m *hoyResolver) invalidate(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.sessions[id]; e != nil {
		m.revoke(e)
		delete(m.sessions, id)
		slog.Info("HOY 来源与签名缓存已清除", "channel", id, "next_action", "下次播放按需获取")
	}
}

func (m *hoyResolver) active(e *hoySession) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[e.channel] != e || e.ctx.Err() != nil {
		return false
	}
	e.lastUsed = m.now()
	return true
}

func (m *hoyResolver) canRefresh(e *hoySession) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.sessions[e.channel]
	return current != nil && current.family == e.family && current.ctx.Err() == nil
}

// Resolve joins one in-flight request per channel. No timer or background
// refresh runs; only playback requests start a bounded API request.
func (m *hoyResolver) resolve(ctx context.Context, ch core.Channel, settings core.Settings, config string) (*hoySession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	code := ch.ProviderChannelID
	if hoyCheckoutID(code) == 0 {
		return nil, errors.New("HOY 仅支持 76、77、78 频道")
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
			return nil, errors.New("HOY 上游拒绝了更新后的会话，请稍后重试或手动刷新来源")
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
			var oldest *hoySession
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
				return nil, errors.New("HOY 解析队列已满，请稍后重试")
			}
			m.revoke(oldest)
			delete(m.sessions, oldest.channel)
		}
		m.serial++
		lifetime, cancel := context.WithCancel(context.Background())
		e = &hoySession{owner: m, id: m.serial, channel: ch.ID, config: config, code: code, ready: make(chan struct{}), ctx: lifetime, cancel: cancel, lastUsed: m.now()}
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

func (m *hoyResolver) initialize(e *hoySession, proxy string) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(e.ctx, 40*time.Second)
	defer cancel()
	mode := "direct"
	if proxy != "" {
		mode = "proxy"
	}
	slog.Info("HOY 来源解析开始", "channel", e.channel, "hoy_channel", e.code, "proxy_mode", mode)
	var err error
	select {
	case m.fetches <- struct{}{}:
		err = m.extract(ctx, e, proxy)
		<-m.fetches
	case <-ctx.Done():
		err = errors.New("HOY 来源请求已取消或超时")
	}
	if e.ctx.Err() != nil {
		err = errors.New("HOY 来源请求已取消")
	}
	if err != nil {
		e.until = m.now().Add(15 * time.Second)
		slog.Warn("HOY 来源解析失败", "channel", e.channel, "duration_ms", time.Since(started).Milliseconds(), "error", err.Error())
	} else {
		slog.Info("HOY 来源解析成功", "channel", e.channel, "hoy_channel", e.code, "duration_ms", time.Since(started).Milliseconds(), "expires_at", e.until)
	}
	e.err = err
	if e.ctx.Err() != nil && e.client != nil {
		e.client.CloseIdleConnections()
	}
	close(e.ready)
}

func hoyCheckoutID(code string) int {
	switch code {
	case "76":
		return 1
	case "77":
		return 2
	case "78":
		return 3
	}
	return 0
}

func hoyRoot(code string) string { return "https://ch" + code + "-live-stream.hoy.tv/ch" + code + "/" }

// Signatures must never leave their channel's CDN directory, including through
// redirects, absolute playlist links, or encoded/traversal paths.
func validateHOYMediaURL(raw, code string) error {
	u, err := url.Parse(raw)
	if err != nil || hoyCheckoutID(code) == 0 || u.Scheme != "https" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") || len(raw) > 16384 {
		return errors.New("HOY 返回的资源地址无效")
	}
	if !strings.EqualFold(u.Hostname(), "ch"+code+"-live-stream.hoy.tv") || !strings.HasPrefix(u.Path, "/ch"+code+"/") || u.RawPath != "" || strings.ContainsAny(u.Path, "\\%") || path.Clean(u.Path) != u.Path {
		return errors.New("HOY 资源必须属于当前频道的媒体目录")
	}
	if _, err := url.ParseQuery(u.RawQuery); err != nil {
		return errors.New("HOY 资源参数无效")
	}
	return nil
}

func validateHOYURL(raw string) error {
	if raw == "https://api2.hoy.tv/api/v3/a/liveCheckout/1" || raw == "https://api2.hoy.tv/api/v3/a/liveCheckout/2" || raw == "https://api2.hoy.tv/api/v3/a/liveCheckout/3" {
		return nil
	}
	for _, code := range []string{"76", "77", "78"} {
		if validateHOYMediaURL(raw, code) == nil {
			return nil
		}
	}
	return errors.New("HOY 来源地址不在允许范围内")
}

func newHOYClient(proxy string) (*http.Client, error) {
	return newProviderClient(proxy, validateHOYURL)
}

type hoySignature struct {
	Policy    string `json:"CloudFront-Policy"`
	KeyPairID string `json:"CloudFront-Key-Pair-Id"`
	Signature string `json:"CloudFront-Signature"`
}

func (s hoySignature) expiry(code string, now time.Time) (time.Time, error) {
	invalid := errors.New("HOY 未返回有效的频道签名或有效期")
	for _, value := range []string{s.Policy, s.KeyPairID, s.Signature} {
		if value == "" || len(value) > 8192 || strings.ContainsAny(value, "\r\n\t ") {
			return time.Time{}, invalid
		}
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.NewReplacer("-", "+", "_", "=", "~", "/").Replace(s.Policy))
	if err != nil {
		return time.Time{}, invalid
	}
	var policy struct {
		Statement []struct {
			Resource  string `json:"Resource"`
			Condition struct {
				Before struct {
					Epoch int64 `json:"AWS:EpochTime"`
				} `json:"DateLessThan"`
				After struct {
					Epoch int64 `json:"AWS:EpochTime"`
				} `json:"DateGreaterThan"`
			} `json:"Condition"`
		} `json:"Statement"`
	}
	if json.Unmarshal(decoded, &policy) != nil || len(policy.Statement) != 1 {
		return time.Time{}, invalid
	}
	statement := policy.Statement[0]
	until := time.Unix(statement.Condition.Before.Epoch, 0)
	if statement.Resource != hoyRoot(code)+"*" || !until.After(now) || time.Unix(statement.Condition.After.Epoch, 0).After(now) {
		return time.Time{}, invalid
	}
	return until, nil
}

func (e *hoySession) signedURL(raw string) (string, error) {
	if err := e.ValidateURL(raw); err != nil {
		return "", err
	}
	u, _ := url.Parse(raw)
	q := u.Query()
	// Replace stale or duplicated authentication supplied by a playlist. Keep
	// other query parameters and sign every resource, regardless of extension.
	q.Set("Policy", e.signed.Policy)
	q.Set("Key-Pair-Id", e.signed.KeyPairID)
	q.Set("Signature", e.signed.Signature)
	u.RawQuery = q.Encode()
	if len(u.String()) > 16384 {
		return "", errors.New("HOY 签名资源地址过长")
	}
	return u.String(), nil
}

func hoyHeaders(req *http.Request, code string) {
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://hoy.tv/live?channel_no="+code)
	req.Header.Set("Origin", "https://hoy.tv")
}

func (m *hoyResolver) extract(ctx context.Context, e *hoySession, proxy string) error {
	base, err := m.client(proxy)
	if err != nil {
		return errors.New("无法创建 HOY 连接，请检查代理配置")
	}
	client := *base
	// The website explicitly sends the returned signature as URL parameters.
	// Do not share or forward API cookies (their .hoy.tv scope spans channels).
	client.Jar = nil
	client.Timeout = 30 * time.Second
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("HOY 重定向过多")
		}
		if len(via) == 0 || via[0].URL.Hostname() == "api2.hoy.tv" {
			return errors.New("HOY 播放接口发生意外重定向")
		}
		if err := e.ValidateURL(req.URL.String()); err != nil {
			return err
		}
		if base.CheckRedirect != nil {
			if err := base.CheckRedirect(req, via); err != nil {
				return err
			}
		}
		raw, err := e.signedURL(req.URL.String())
		if err != nil {
			return err
		}
		req.URL, _ = url.Parse(raw)
		hoyHeaders(req, e.code)
		return nil
	}
	e.client = &client
	endpoint := "https://api2.hoy.tv/api/v3/a/liveCheckout/" + strconv.Itoa(hoyCheckoutID(e.code))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	hoyHeaders(req, e.code)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HOY 接口请求失败（%s），请检查出口、代理或网络", providerNetworkReason(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HOY 接口返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024+1))
	if err != nil || len(body) > 1024*1024 {
		return errors.New("HOY 接口响应不完整或过大")
	}
	var result struct {
		Code int `json:"code"`
		Data struct {
			ID    int `json:"id"`
			Video struct {
				ID   int    `json:"id"`
				Link string `json:"link"`
			} `json:"video"`
			Signed hoySignature `json:"signed"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &result) != nil || result.Code != 200 || result.Data.ID != hoyCheckoutID(e.code) || strconv.Itoa(result.Data.Video.ID) != e.code {
		return errors.New("HOY 未返回有效的频道播放信息")
	}
	if err := e.ValidateURL(result.Data.Video.Link); err != nil {
		return err
	}
	u, _ := url.Parse(result.Data.Video.Link)
	if !strings.HasSuffix(u.Path, ".m3u8") {
		return errors.New("HOY 此频道未提供 HLS 直播来源")
	}
	now := m.now()
	until, err := result.Data.Signed.expiry(e.code, now)
	if err != nil {
		return err
	}
	// Refresh slightly before expiry; also cap reuse to one hour as for TVB.
	e.until = until.Add(-30 * time.Second)
	if !e.until.After(now) {
		return errors.New("HOY 接口返回的签名即将过期")
	}
	if limit := now.Add(time.Hour); limit.Before(e.until) {
		e.until = limit
	}
	e.signed = result.Data.Signed
	e.root, err = e.signedURL(result.Data.Video.Link)
	return err
}

func (e *hoySession) Fetch(ctx context.Context, raw, rangeHeader string) (*http.Response, error) {
	if !e.Active() {
		return nil, ErrRevoked
	}
	raw, err := e.signedURL(raw)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	stop := context.AfterFunc(e.ctx, cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		stop()
		cancel()
		return nil, errors.New("HOY 资源请求无效")
	}
	hoyHeaders(req, e.code)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		stop()
		cancel()
		if !e.Active() {
			return nil, ErrRevoked
		}
		slog.Warn("HOY 媒体请求失败", "channel", e.channel, "host", req.URL.Hostname(), "reason", providerNetworkReason(err))
		return nil, errors.New("HOY 媒体请求失败，请检查出口和网络")
	}
	resp.Body = &providerResponseBody{ReadCloser: resp.Body, stop: stop, cancel: cancel}
	return resp, nil
}

func (m *hoyResolver) Resolve(ctx context.Context, ch core.Channel, settings core.Settings, config string) (Playback, error) {
	e, err := m.resolve(ctx, ch, settings, config)
	if err != nil {
		return Playback{}, err
	}
	return Playback{URL: e.root, Session: e}, nil
}
func (m *hoyResolver) Invalidate(id string)        { m.invalidate(id) }
func (e *hoySession) ID() string                   { return "hoy-" + strconv.FormatUint(e.id, 10) }
func (e *hoySession) URL() string                  { return e.root }
func (e *hoySession) Active() bool                 { return e.owner.active(e) }
func (e *hoySession) CanRefresh() bool             { return e.owner.canRefresh(e) }
func (e *hoySession) Expired() bool                { return e.expired(e.owner.now()) }
func (e *hoySession) Reject()                      { e.stale.Store(true) }
func (e *hoySession) ValidateURL(raw string) error { return validateHOYMediaURL(raw, e.code) }

func hoyDefinition() definition {
	return definition{catalog: Catalog{
		ID: "hoy", Name: "HOY", Website: "https://hoy.tv/live?channel_no=76",
		Description: "HOY 76、77、78。播放时按需获取来源，默认服务器中继；需要直播来源允许的网络出口。刷新来源可清除播放缓存。",
		SourceType:  "builtin", DefaultMode: "relay", LinkLabel: "官网直播 ↗",
		PlaybackHelp: "推荐中继；直连需要播放器将签名用于后续清单和分片，且客户端出口符合来源地区限制。",
		Channels: []Channel{
			{ID: "76", Name: "HOY 76", URL: "https://hoy.tv/live?channel_no=76", Selected: true, Note: "HOY 76 台"},
			{ID: "77", Name: "HOY 77", URL: "https://hoy.tv/live?channel_no=77", Selected: true, Note: "HOY 77 台"},
			{ID: "78", Name: "HOY 78", URL: "https://hoy.tv/live?channel_no=78", Selected: true, Note: "HOY 78 台"},
		},
	}, create: func(opts Options) resolver {
		m := newHOYResolver(opts.ClientFactory)
		if opts.Now != nil {
			m.now = opts.Now
		}
		return m
	}}
}
