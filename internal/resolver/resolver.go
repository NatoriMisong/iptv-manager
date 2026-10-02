// Package resolver obtains temporary YouTube HLS URLs without processing video.
package resolver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"iptv-manager/internal/core"
)

var (
	ErrOffline = errors.New("频道当前未直播")
	ErrNoHLS   = errors.New("没有符合清晰度限制的可用 HLS 来源；独立音轨需要有效的 HLS 主清单，请查看格式筛选日志")
	ErrBusy    = errors.New("解析队列已满，请稍后重试")
)

type Result struct {
	URL string
	// VideoURL selects a video rendition in URL's master playlist. The media
	// layer must verify its AUDIO group before serving either playback mode.
	VideoURL    string
	VideoFormat VideoFormat
	Headers     map[string]string
	Title       string
	Height      int
	ExpiresAt   time.Time
}

// VideoFormat identifies the rendition independently of its temporary URL.
// The media layer still obtains the audio association from the current master.
type VideoFormat struct {
	ID    string
	Codec string
	Width int
	FPS   float64
}

type Options struct {
	Command     string
	CookiesFile string
	JSRuntime   string
	Timeout     time.Duration
	Logger      *slog.Logger
}

type cacheKey struct {
	id, source, proxy string
	quality           int
	generation        uint64
}

type cacheEntry struct {
	result Result
	err    error
	until  time.Time
}

type flight struct {
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	waiters int
	result  Result
	err     error
}

type commandRunner func(context.Context, string, []string) ([]byte, []byte, error)

type Resolver struct {
	opts       Options
	logger     *slog.Logger
	mu         sync.Mutex
	cache      map[cacheKey]cacheEntry
	inflight   map[cacheKey]*flight
	generation map[string]uint64
	latest     map[string]cacheKey
	statuses   map[string]core.ChannelStatus
	process    chan struct{}
	queue      chan struct{}
	run        commandRunner
	now        func() time.Time
}

func New(opts Options) *Resolver {
	if opts.Command == "" {
		opts.Command = "yt-dlp"
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 45 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Resolver{
		opts: opts, logger: opts.Logger.With("component", "resolver"), cache: make(map[cacheKey]cacheEntry),
		inflight: make(map[cacheKey]*flight), generation: make(map[string]uint64),
		latest: make(map[string]cacheKey), statuses: make(map[string]core.ChannelStatus),
		process: make(chan struct{}, 1), queue: make(chan struct{}, 8),
		run: runCommand, now: time.Now,
	}
}

// Resolve coalesces identical requests. A cancelled caller does not interrupt
// another viewer's request; the subprocess stops when its last caller leaves.
func (r *Resolver) Resolve(ctx context.Context, ch core.Channel, settings core.Settings) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if !validSource(ch.URL) {
		r.logger.Warn("直播来源解析未启动", "channel", ch.ID, "reason", "invalid_source")
		return Result{}, errors.New("频道来源必须是有效的 YouTube HTTPS 地址")
	}
	proxy, err := effectiveProxy(ch, settings)
	if err != nil {
		r.logger.Warn("直播来源解析未启动", "channel", ch.ID, "reason", "invalid_proxy")
		return Result{}, err
	}
	quality := ch.Quality
	if quality <= 0 {
		quality = settings.DefaultQuality
	}
	if quality <= 0 {
		quality = 720
	}

	r.mu.Lock()
	k := cacheKey{id: ch.ID, source: ch.URL, proxy: proxy, quality: quality, generation: r.generation[ch.ID]}
	r.latest[ch.ID] = k
	if cached, ok := r.cache[k]; ok {
		if r.now().Before(cached.until) {
			r.setStatusLocked(k, cached.result, cached.err)
			r.mu.Unlock()
			r.logFor(k).Debug("命中直播来源缓存", "success", cached.err == nil, "cache_until", cached.until)
			return cloneResult(cached.result), cached.err
		}
		delete(r.cache, k)
	}
	f, ok := r.inflight[k]
	if ok {
		f.waiters++
	} else {
		select {
		case r.queue <- struct{}{}:
		default:
			r.setStatusLocked(k, Result{}, ErrBusy)
			r.mu.Unlock()
			r.logFor(k).Warn("直播来源解析未启动", "reason", "queue_full")
			return Result{}, ErrBusy
		}
		flightCtx, cancel := context.WithTimeout(context.Background(), r.opts.Timeout)
		f = &flight{ctx: flightCtx, cancel: cancel, done: make(chan struct{}), waiters: 1}
		r.inflight[k] = f
		r.statuses[ch.ID] = core.ChannelStatus{State: "resolving", Message: "正在解析直播来源", UpdatedAt: r.now()}
		go r.resolveFlight(k, f)
	}
	r.mu.Unlock()

	select {
	case <-ctx.Done():
		r.releaseWaiter(k, f)
		return Result{}, ctx.Err()
	case <-f.done:
		return cloneResult(f.result), f.err
	}
}

func (r *Resolver) releaseWaiter(k cacheKey, f *flight) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inflight[k] != f {
		return
	}
	f.waiters--
	if f.waiters == 0 {
		delete(r.inflight, k)
		f.cancel()
		if r.latest[k.id] == k {
			r.statuses[k.id] = core.ChannelStatus{State: "unknown", Message: "解析已取消", UpdatedAt: r.now()}
		}
	}
}

func (r *Resolver) resolveFlight(k cacheKey, f *flight) {
	defer func() { <-r.queue; f.cancel() }()
	started := time.Now()
	logger := r.logFor(k)
	logger.Info("直播来源解析开始", "quality_limit", k.quality, "proxy_mode", proxyMode(k.proxy), "cookies_enabled", r.opts.CookiesFile != "")
	var result Result
	var err error
	select {
	case r.process <- struct{}{}:
		if f.ctx.Err() != nil {
			err = f.ctx.Err()
		} else {
			logger.Debug("yt-dlp 开始执行", "queue_ms", time.Since(started).Milliseconds())
			result, err = r.extract(f.ctx, k)
		}
		<-r.process
	case <-f.ctx.Done():
		err = f.ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		err = errors.New("直播来源解析超时，请稍后重试")
	}
	switch {
	case err == nil:
		upstream, _ := url.Parse(result.URL)
		logger.Info("直播来源解析成功", "duration_ms", time.Since(started).Milliseconds(), "height", result.Height, "source_host", upstream.Hostname(), "expires_at", result.ExpiresAt, "verify_audio_group", result.VideoURL != "", "format_id", diagnosticLabel(result.VideoFormat.ID), "video_codec", diagnosticLabel(result.VideoFormat.Codec))
	case errors.Is(err, context.Canceled):
		logger.Info("直播来源解析取消", "duration_ms", time.Since(started).Milliseconds(), "reason", "所有等待请求已取消或频道配置已更新")
	default:
		logger.Warn("直播来源解析失败", "duration_ms", time.Since(started).Milliseconds(), "error", err.Error())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f.result, f.err = result, err
	if r.inflight[k] == f {
		delete(r.inflight, k)
		if r.generation[k.id] == k.generation && !errors.Is(f.ctx.Err(), context.Canceled) {
			until := r.now().Add(15 * time.Second)
			if err == nil {
				remaining := result.ExpiresAt.Sub(r.now())
				margin := time.Minute
				if remaining < 2*time.Minute {
					margin = remaining / 3
				}
				until = result.ExpiresAt.Add(-margin)
			}
			// A channel edited during a request cannot be overwritten by that request.
			if r.latest[k.id] == k {
				for old := range r.cache {
					if old.id == k.id {
						delete(r.cache, old)
					}
				}
				r.cache[k] = cacheEntry{result: result, err: err, until: until}
				r.setStatusLocked(k, result, err)
			}
		}
	}
	close(f.done)
}

// Invalidate also cancels old work so it cannot restore a stale cache entry.
func (r *Resolver) Invalidate(id string) {
	r.logger.Info("直播来源缓存已清除", "channel", id, "next_action", "下次播放请求将重新解析")
	r.mu.Lock()
	defer r.mu.Unlock()
	r.generation[id]++
	delete(r.latest, id)
	for k := range r.cache {
		if k.id == id {
			delete(r.cache, k)
		}
	}
	for k, f := range r.inflight {
		if k.id == id {
			f.cancel()
			delete(r.inflight, k)
		}
	}
	r.statuses[id] = core.ChannelStatus{State: "unknown", Message: "等待解析", UpdatedAt: r.now()}
}

func (r *Resolver) Statuses() map[string]core.ChannelStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	copy := make(map[string]core.ChannelStatus, len(r.statuses))
	for id, status := range r.statuses {
		if status.State == "ready" && !r.now().Before(status.ExpiresAt) {
			status.State, status.Message = "unknown", "来源已到期，等待重新解析"
		}
		copy[id] = status
	}
	return copy
}

func (r *Resolver) setStatusLocked(k cacheKey, result Result, err error) {
	status := core.ChannelStatus{State: "ready", Message: "直播来源可用", Title: result.Title, Height: result.Height, UpdatedAt: r.now(), ExpiresAt: result.ExpiresAt}
	if err != nil {
		status.State, status.Message = "error", err.Error()
		if errors.Is(err, ErrOffline) {
			status.State = "offline"
		}
	}
	r.statuses[k.id] = status
}

func (r *Resolver) extract(ctx context.Context, k cacheKey) (Result, error) {
	logger := r.logFor(k)
	sensitive := diagnosticSecrets(k.proxy, r.opts.CookiesFile)
	args := []string{"--ignore-config", "--no-playlist", "--skip-download", "--dump-single-json", "--no-progress", "--no-cache-dir", "--socket-timeout", "15", "--retries", "1", "--extractor-retries", "1", "--proxy", k.proxy}
	if r.opts.CookiesFile != "" {
		cookies, err := copyCookiesFile(r.opts.CookiesFile)
		if err != nil {
			return Result{}, err
		}
		defer os.Remove(cookies)
		sensitive = append(sensitive, cookies)
		args = append(args, "--cookies", cookies)
	}
	if r.opts.JSRuntime != "" {
		args = append(args, "--js-runtimes", r.opts.JSRuntime)
	}
	args = append(args, "--", k.source)
	stdout, stderr, err := r.run(ctx, r.opts.Command, args)
	if diagnostic := sanitizeDiagnostic(string(stderr), sensitive...); diagnostic != "" {
		logger.Warn("yt-dlp 诊断输出", "failed", err != nil, "detail", diagnostic)
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	if err != nil {
		logger.Warn("yt-dlp 执行失败", "reason", commandErrorKind(err), "exit_code", commandExitCode(err))
		// Do not return raw process errors or output: they may contain cookies,
		// proxy credentials, or temporary signed URLs.
		message := strings.ToLower(string(stderr))
		switch {
		case strings.Contains(message, "not currently live"), strings.Contains(message, "live event will begin"), strings.Contains(message, "live event has ended"), strings.Contains(message, "premieres in"):
			return Result{}, ErrOffline
		case strings.Contains(message, "sign in"), strings.Contains(message, "sign-in"), strings.Contains(message, "not a bot"):
			return Result{}, errors.New("YouTube 要求额外验证，请检查服务器出口或解析器配置")
		case errors.Is(err, exec.ErrNotFound):
			return Result{}, errors.New("找不到 yt-dlp，请检查安装或容器配置")
		default:
			return Result{}, errors.New("yt-dlp 解析失败，请检查来源、网络或解析器版本")
		}
	}
	result, err := parseResult(stdout, k.quality, r.now())
	if errors.Is(err, ErrNoHLS) {
		logFormatDiagnostics(logger, stdout, k.quality)
	}
	return result, err
}

// yt-dlp updates its cookie jar on exit. Work from a private disposable copy,
// allowing operators to mount the original credential file read-only.
func copyCookiesFile(path string) (string, error) {
	const maximum = 1 << 20
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("无法读取 Cookies 文件，请检查路径和权限")
	}
	if info.Size() > maximum {
		return "", errors.New("Cookies 文件不能超过 1 MiB")
	}
	source, err := os.Open(path)
	if err != nil {
		return "", errors.New("无法读取 Cookies 文件，请检查路径和权限")
	}
	defer source.Close()
	temporary, err := os.CreateTemp("", "iptv-manager-cookies-*")
	if err != nil {
		return "", errors.New("无法创建 Cookies 临时副本，请检查临时目录权限")
	}
	name := temporary.Name()
	n, copyErr := io.Copy(temporary, io.LimitReader(source, maximum+1))
	closeErr := temporary.Close()
	if copyErr != nil || closeErr != nil || n > maximum {
		_ = os.Remove(name)
		if n > maximum {
			return "", errors.New("Cookies 文件不能超过 1 MiB")
		}
		return "", errors.New("无法复制 Cookies 文件，请检查文件和临时目录权限")
	}
	return name, nil
}

type format struct {
	ID          string            `json:"format_id"`
	URL         string            `json:"url"`
	ManifestURL string            `json:"manifest_url"`
	Protocol    string            `json:"protocol"`
	VCodec      string            `json:"vcodec"`
	ACodec      string            `json:"acodec"`
	Height      int               `json:"height"`
	Width       int               `json:"width"`
	FPS         float64           `json:"fps"`
	TBR         float64           `json:"tbr"`
	HTTPHeaders map[string]string `json:"http_headers"`
}

type videoInfo struct {
	format
	Title      string   `json:"title"`
	IsLive     *bool    `json:"is_live"`
	LiveStatus string   `json:"live_status"`
	Formats    []format `json:"formats"`
}

func parseResult(data []byte, quality int, now time.Time) (Result, error) {
	var info videoInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return Result{}, errors.New("解析器未返回有效的直播数据")
	}
	if (info.IsLive == nil || !*info.IsLive) && info.LiveStatus != "is_live" {
		if (info.IsLive != nil && !*info.IsLive) || info.LiveStatus == "not_live" || info.LiveStatus == "was_live" || info.LiveStatus == "post_live" || info.LiveStatus == "is_upcoming" {
			return Result{}, ErrOffline
		}
		return Result{}, errors.New("解析器未提供有效的直播状态，请检查来源、网络或额外验证要求")
	}
	formats := append(info.Formats, info.format)
	var best *format
	for i := range formats {
		f := &formats[i]
		if f.Protocol != "m3u8" && f.Protocol != "m3u8_native" {
			continue
		}
		if !validMediaURL(f.URL) || !knownCodec(f.VCodec) || !knownCodec(f.ACodec) {
			continue
		}
		if f.Height <= 0 || f.Height > quality {
			continue
		}
		if best == nil || betterFormat(*f, *best) {
			best = f
		}
	}
	if best == nil {
		// A video-only HLS rendition is usable through its original master,
		// whose AUDIO association will be verified before exposing the source.
		// Never infer an audio track from format IDs or combine arbitrary URLs.
		for i := range formats {
			f := &formats[i]
			if (f.Protocol != "m3u8" && f.Protocol != "m3u8_native") || !validMediaURL(f.URL) || !validMediaURL(f.ManifestURL) || f.URL == f.ManifestURL || !knownCodec(f.VCodec) || f.Height <= 0 || f.Height > quality {
				continue
			}
			if best == nil || betterVideoFormat(*f, *best) {
				best = f
			}
		}
		if best == nil {
			return Result{}, ErrNoHLS
		}
		expires := sourceExpiry(best.ManifestURL, now)
		if videoExpiry := sourceExpiry(best.URL, now); videoExpiry.Before(expires) {
			expires = videoExpiry
		}
		if !expires.After(now) {
			return Result{}, errors.New("解析器返回的直播来源已经过期")
		}
		return Result{URL: best.ManifestURL, VideoURL: best.URL, VideoFormat: VideoFormat{ID: best.ID, Codec: best.VCodec, Width: best.Width, FPS: best.FPS}, Headers: safeHeaders(info.HTTPHeaders, best.HTTPHeaders), Title: info.Title, Height: best.Height, ExpiresAt: expires}, nil
	}
	expires := sourceExpiry(best.URL, now)
	if !expires.After(now) {
		return Result{}, errors.New("解析器返回的直播来源已经过期")
	}
	return Result{URL: best.URL, Headers: safeHeaders(info.HTTPHeaders, best.HTTPHeaders), Title: info.Title, Height: best.Height, ExpiresAt: expires}, nil
}

func betterVideoFormat(a, b format) bool {
	h264 := func(codec string) bool {
		codec = strings.ToLower(codec)
		return strings.HasPrefix(codec, "avc") || strings.HasPrefix(codec, "h264")
	}
	if h264(a.VCodec) != h264(b.VCodec) {
		return h264(a.VCodec)
	}
	if a.Height != b.Height {
		return a.Height > b.Height
	}
	return a.TBR > b.TBR
}

func betterFormat(a, b format) bool {
	compatible := func(f format) bool {
		v, a := strings.ToLower(f.VCodec), strings.ToLower(f.ACodec)
		return (strings.HasPrefix(v, "avc") || strings.HasPrefix(v, "h264")) && (strings.HasPrefix(a, "mp4a") || strings.HasPrefix(a, "aac"))
	}
	if compatible(a) != compatible(b) {
		return compatible(a)
	}
	if a.Height != b.Height {
		return a.Height > b.Height
	}
	return a.TBR > b.TBR
}

func knownCodec(codec string) bool { return codec != "" && codec != "none" && codec != "unknown" }

func validSource(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "youtube.com" || h == "www.youtube.com" || h == "m.youtube.com" || h == "youtu.be"
}

func validMediaURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Port() != "" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "googlevideo.com" || strings.HasSuffix(h, ".googlevideo.com") || h == "youtube.com" || strings.HasSuffix(h, ".youtube.com")
}

func effectiveProxy(ch core.Channel, settings core.Settings) (string, error) {
	proxy := strings.TrimSpace(core.EffectiveProxy(ch, settings))
	if proxy == "" {
		return "", nil
	}
	u, err := url.Parse(proxy)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("出站代理配置无效")
	}
	return proxy, nil
}

func safeHeaders(sources ...map[string]string) map[string]string {
	headers := make(map[string]string)
	for _, source := range sources {
		for key, value := range source {
			key = http.CanonicalHeaderKey(key)
			if strings.ContainsAny(value, "\r\n") {
				continue
			}
			switch key {
			case "User-Agent", "Accept", "Accept-Language":
				headers[key] = value
			case "Referer", "Origin":
				if validSource(value) {
					headers[key] = value
				}
			}
		}
	}
	return headers
}

func sourceExpiry(raw string, now time.Time) time.Time {
	u, err := url.Parse(raw)
	if err != nil {
		return now.Add(5 * time.Minute)
	}
	candidates := []string{u.Query().Get("expire"), u.Query().Get("expires"), u.Query().Get("expiration")}
	parts := strings.Split(u.Path, "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "expire" || parts[i] == "expires" {
			candidates = append(candidates, parts[i+1])
		}
	}
	var expires time.Time
	for _, candidate := range candidates {
		if seconds, err := strconv.ParseInt(candidate, 10, 64); err == nil && seconds > 0 {
			t := time.Unix(seconds, 0)
			if expires.IsZero() || t.Before(expires) {
				expires = t
			}
		}
	}
	if expires.IsZero() {
		return now.Add(5 * time.Minute)
	}
	return expires
}

func cloneResult(in Result) Result {
	out := in
	out.Headers = make(map[string]string, len(in.Headers))
	for k, v := range in.Headers {
		out.Headers[k] = v
	}
	return out
}

// boundedBuffer keeps subprocess output from consuming all available memory.
// It continues accepting bytes after its cap, so a noisy child cannot deadlock.
type boundedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.Len()
	if remaining < n {
		b.overflow = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func runCommand(ctx context.Context, command string, args []string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	configureProcess(cmd)
	defer cleanupProcess(cmd)
	stdout := &boundedBuffer{limit: 16 << 20}
	stderr := &boundedBuffer{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if stdout.overflow {
		return nil, stderr.Bytes(), fmt.Errorf("extractor output exceeded limit")
	}
	return stdout.Bytes(), stderr.Bytes(), err
}
