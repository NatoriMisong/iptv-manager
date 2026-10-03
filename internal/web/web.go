// Package web provides the local administration UI and authenticated API.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"iptv-manager/internal/core"
	"iptv-manager/internal/provider"
	"iptv-manager/internal/store"
	"iptv-manager/internal/subscription"
)

//go:embed static/*
var assets embed.FS

type Repository interface {
	Channels(context.Context) ([]core.Channel, error)
	Channel(context.Context, string) (core.Channel, error)
	SaveChannel(context.Context, core.Channel) (core.Channel, error)
	AddChannels(context.Context, core.BulkChannelRequest) (core.BulkChannelResult, error)
	DeleteChannel(context.Context, string) error
	Reorder(context.Context, []string) error
	Settings(context.Context) (core.Settings, error)
	SaveSettings(context.Context, core.Settings) error
	AdminHash(context.Context) (string, error)
	SetAdminHash(context.Context, string) error
	Export(context.Context) (core.Backup, error)
	Import(context.Context, core.Backup) error
	Traffic(context.Context, string) (core.Traffic, error)
	Subscriptions(context.Context) ([]core.Subscription, error)
	SaveSubscription(context.Context, core.Subscription) (core.Subscription, error)
	DeleteSubscription(context.Context, string) error
	SaveProxy(context.Context, core.Proxy) (core.Proxy, error)
	DeleteProxy(context.Context, string) error
	SetProviderProxy(context.Context, string, string) error
}

type Media interface {
	Statuses() map[string]core.ChannelStatus
	Invalidate(string)
}

type Options struct {
	SecureCookies    bool
	Version          string
	SyncSubscription func(context.Context, string) error
	// ProxyTester returns the egress IP seen through the proxy URL. Tests inject
	// a fake; production fetches ipip.info.
	ProxyTester func(context.Context, string) (string, error)
}

type session struct {
	csrf    string
	expires time.Time
}

type attempt struct {
	count int
	start time.Time
}

type server struct {
	repo         Repository
	media        Media
	opts         Options
	credentialMu sync.Mutex
	mu           sync.Mutex
	sessions     map[string]session
	attempts     map[string]attempt
	proxyTests   chan struct{}
}

const sessionCookie = "iptv_session"
const sessionLifetime = 8 * time.Hour
const maxBody = 1 << 20

func New(repo Repository, media Media, opts Options) (http.Handler, error) {
	if repo == nil || media == nil {
		return nil, errors.New("repository and media are required")
	}
	s := &server{repo: repo, media: media, opts: opts, sessions: make(map[string]session), attempts: make(map[string]attempt), proxyTests: make(chan struct{}, 1)}
	static, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.auth(s.logout))
	mux.HandleFunc("GET /api/state", s.auth(s.state))
	mux.HandleFunc("GET /api/builtin-sources", s.auth(s.builtinSources))
	mux.HandleFunc("POST /api/channels", s.auth(s.saveChannel))
	mux.HandleFunc("POST /api/channels/bulk", s.auth(s.addChannels))
	mux.HandleFunc("POST /api/subscriptions", s.auth(s.saveSubscription))
	mux.HandleFunc("PUT /api/subscriptions/{id}", s.auth(s.saveSubscription))
	mux.HandleFunc("DELETE /api/subscriptions/{id}", s.auth(s.deleteSubscription))
	mux.HandleFunc("POST /api/subscriptions/{id}/sync", s.auth(s.syncSubscription))
	mux.HandleFunc("PUT /api/channels/{id}", s.auth(s.saveChannel))
	mux.HandleFunc("DELETE /api/channels/{id}", s.auth(s.deleteChannel))
	mux.HandleFunc("POST /api/channels/{id}/refresh", s.auth(s.refresh))
	mux.HandleFunc("POST /api/reorder", s.auth(s.reorder))
	mux.HandleFunc("PUT /api/settings", s.auth(s.settings))
	mux.HandleFunc("GET /api/backup", s.auth(s.backup))
	mux.HandleFunc("POST /api/restore", s.auth(s.restore))
	mux.HandleFunc("POST /api/password", s.auth(s.password))
	mux.HandleFunc("POST /api/token", s.auth(s.rotateToken))
	mux.HandleFunc("POST /api/proxies", s.auth(s.saveProxy))
	mux.HandleFunc("PUT /api/proxies/{id}", s.auth(s.saveProxy))
	mux.HandleFunc("DELETE /api/proxies/{id}", s.auth(s.deleteProxy))
	mux.HandleFunc("POST /api/proxies/{id}/test", s.auth(s.testProxy))
	mux.HandleFunc("PUT /api/providers/{id}/proxy", s.auth(s.providerProxy))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { fail(w, 404, "接口不存在") })
	files := http.FileServer(http.FS(static))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/" && r.URL.Path != "/app.js" && r.URL.Path != "/style.css" {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: http: https:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	}), nil
}

func token() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]string{"error": message})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	return decodeLimit(w, r, v, maxBody)
}
func decodeLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		fail(w, 415, "请使用 JSON 请求")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, 400, "请求内容无效或超过大小限制")
		return false
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		fail(w, 400, "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}

func sameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Path == "" && strings.EqualFold(u.Host, r.Host)
}

func (s *server) auth(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			fail(w, 401, "请先登录")
			return
		}
		s.mu.Lock()
		sess, ok := s.sessions[c.Value]
		if ok && !time.Now().Before(sess.expires) {
			delete(s.sessions, c.Value)
			ok = false
		}
		s.mu.Unlock()
		if !ok {
			fail(w, 401, "登录已过期，请重新登录")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !sameOrigin(r) || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(sess.csrf)) != 1 {
				fail(w, 403, "请求验证失败，请刷新页面")
				return
			}
		}
		next(w, r)
	}
}

func (s *server) allowLogin(r *http.Request) bool {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, a := range s.attempts {
		if now.Sub(a.start) > 15*time.Minute {
			delete(s.attempts, key)
		}
	}
	a := s.attempts[ip]
	if a.start.IsZero() {
		a.start = now
	}
	if a.count >= 10 || (len(s.attempts) >= 4096 && a.count == 0) {
		return false
	}
	a.count++
	s.attempts[ip] = a
	return true
}

func (s *server) cookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: value, Path: "/", HttpOnly: true, Secure: s.opts.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		fail(w, 403, "请求来源无效")
		return
	}
	if !s.allowLogin(r) {
		w.Header().Set("Retry-After", "900")
		fail(w, 429, "登录尝试过多，请稍后重试")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	if len(body.Password) > 72 {
		fail(w, 401, "密码错误")
		return
	}
	// Keep a login using the previous password from creating a session after
	// a concurrent password change has revoked all existing sessions.
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	hash, err := s.repo.AdminHash(r.Context())
	if err != nil || hash == "" {
		fail(w, 503, "管理账户尚未就绪")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.Password)) != nil {
		fail(w, 401, "密码错误")
		return
	}
	id, err := token()
	if err != nil {
		fail(w, 500, "无法创建会话")
		return
	}
	csrf, err := token()
	if err != nil {
		fail(w, 500, "无法创建会话")
		return
	}
	s.mu.Lock()
	now := time.Now()
	for k, v := range s.sessions {
		if now.After(v.expires) {
			delete(s.sessions, k)
		}
	}
	if len(s.sessions) >= 128 {
		var oldest string
		var expiry time.Time
		for k, v := range s.sessions {
			if oldest == "" || v.expires.Before(expiry) {
				oldest = k
				expiry = v.expires
			}
		}
		delete(s.sessions, oldest)
	}
	s.sessions[id] = session{csrf: csrf, expires: now.Add(sessionLifetime)}
	s.mu.Unlock()
	s.cookie(w, id, int(sessionLifetime.Seconds()))
	respond(w, 200, map[string]string{"csrf": csrf})
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	c, _ := r.Cookie(sessionCookie)
	s.mu.Lock()
	delete(s.sessions, c.Value)
	s.mu.Unlock()
	s.cookie(w, "", -1)
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *server) state(w http.ResponseWriter, r *http.Request) {
	channels, err := s.repo.Channels(r.Context())
	if err != nil {
		fail(w, 500, "无法读取频道")
		return
	}
	settings, err := s.repo.Settings(r.Context())
	if err != nil {
		fail(w, 500, "无法读取设置")
		return
	}
	traffic, err := s.repo.Traffic(r.Context(), time.Now().UTC().Format("2006-01"))
	if err != nil {
		fail(w, 500, "无法读取流量")
		return
	}
	subs, err := s.repo.Subscriptions(r.Context())
	if err != nil {
		fail(w, 500, "无法读取 M3U 订阅")
		return
	}
	c, _ := r.Cookie(sessionCookie)
	s.mu.Lock()
	csrf := s.sessions[c.Value].csrf
	s.mu.Unlock()
	respond(w, 200, map[string]any{"channels": channels, "builtin_sources": provider.Catalogs(), "subscriptions": subs, "settings": settings, "statuses": s.media.Statuses(), "traffic": traffic, "version": s.opts.Version, "csrf": csrf})
}

func (s *server) saveChannel(w http.ResponseWriter, r *http.Request) {
	var ch core.Channel
	if !decode(w, r, &ch) {
		return
	}
	ch.ID = r.PathValue("id")
	if ch.ID != "" {
		if _, err := s.repo.Channel(r.Context(), ch.ID); err != nil {
			fail(w, 404, "频道不存在")
			return
		}
	}
	saved, err := s.repo.SaveChannel(r.Context(), ch)
	if err != nil {
		fail(w, 400, "频道保存失败，请检查名称、来源类型、直播链接、画质和代理选择")
		return
	}
	s.media.Invalidate(saved.ID)
	respond(w, 200, saved)
}

func (s *server) saveSubscription(w http.ResponseWriter, r *http.Request) {
	var sub core.Subscription
	if !decode(w, r, &sub) {
		return
	}
	sub.ID = r.PathValue("id")
	saved, err := s.repo.SaveSubscription(r.Context(), sub)
	if err != nil {
		if errors.Is(err, store.ErrValidation) {
			fail(w, 400, strings.TrimPrefix(err.Error(), store.ErrValidation.Error()+": "))
		} else if errors.Is(err, store.ErrNotFound) {
			fail(w, 404, "订阅不存在")
		} else {
			fail(w, 500, "订阅保存失败")
		}
		return
	}
	respond(w, 200, saved)
}
func (s *server) deleteSubscription(w http.ResponseWriter, r *http.Request) {
	channels, err := s.repo.Channels(r.Context())
	if err != nil {
		fail(w, 500, "无法读取频道")
		return
	}
	if err := s.repo.DeleteSubscription(r.Context(), r.PathValue("id")); err != nil {
		fail(w, 400, "订阅删除失败")
		return
	}
	for _, ch := range channels {
		if ch.SubscriptionID == r.PathValue("id") {
			s.media.Invalidate(ch.ID)
		}
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func (s *server) syncSubscription(w http.ResponseWriter, r *http.Request) {
	if s.opts.SyncSubscription == nil {
		fail(w, 503, "订阅同步暂不可用")
		return
	}
	if err := s.opts.SyncSubscription(r.Context(), r.PathValue("id")); err != nil {
		code := 502
		if errors.Is(err, subscription.ErrBusy) {
			code = 409
		}
		fail(w, code, err.Error())
		return
	}
	respond(w, 200, map[string]string{"message": "订阅同步完成"})
}

func (s *server) addChannels(w http.ResponseWriter, r *http.Request) {
	var req core.BulkChannelRequest
	if !decode(w, r, &req) {
		return
	}
	result, err := s.repo.AddChannels(r.Context(), req)
	if err != nil {
		if errors.Is(err, store.ErrValidation) {
			fail(w, 400, strings.TrimPrefix(err.Error(), store.ErrValidation.Error()+": "))
		} else {
			fail(w, 500, "批量保存失败，本批次未写入，请稍后重试")
		}
		return
	}
	respond(w, 200, result)
}

func (s *server) deleteChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.repo.DeleteChannel(r.Context(), id); err != nil {
		fail(w, 400, "无法删除频道")
		return
	}
	s.media.Invalidate(id)
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *server) refresh(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ch, err := s.repo.Channel(r.Context(), id)
	if err != nil {
		fail(w, 404, "频道不存在")
		return
	}
	s.media.Invalidate(id)
	if ch.IsBuiltin() {
		respond(w, 200, map[string]string{"message": "已清除内置来源的播放地址、会话 Cookie 和媒体缓存，下次播放时重新获取"})
		return
	}
	if ch.IsStream() {
		respond(w, 200, map[string]string{"message": "已清除播放状态，下次播放重新连接来源"})
		return
	}
	respond(w, 200, map[string]string{"message": "已清除来源缓存，下次播放时重新解析"})
}

func (s *server) reorder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := s.repo.Reorder(r.Context(), body.IDs); err != nil {
		fail(w, 400, "频道排序无效，请刷新后重试")
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *server) invalidateAll(ctx context.Context) {
	channels, err := s.repo.Channels(ctx)
	if err != nil {
		return
	}
	for _, ch := range channels {
		s.media.Invalidate(ch.ID)
	}
}

func (s *server) settings(w http.ResponseWriter, r *http.Request) {
	var setting core.Settings
	if !decode(w, r, &setting) {
		return
	}
	current, err := s.repo.Settings(r.Context())
	if err != nil {
		fail(w, 500, "无法读取设置")
		return
	}
	// The settings form never carries the token or proxy configuration.
	setting.PlaybackToken = current.PlaybackToken
	setting.Proxies, setting.ProviderProxies, setting.LegacyProxy = current.Proxies, current.ProviderProxies, ""
	if err := s.repo.SaveSettings(r.Context(), setting); err != nil {
		fail(w, 400, "设置无效，请检查访问地址、画质和流量预算")
		return
	}
	s.invalidateAll(r.Context())
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *server) backup(w http.ResponseWriter, r *http.Request) {
	b, err := s.repo.Export(r.Context())
	if err != nil {
		fail(w, 500, "导出失败")
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="iptv-manager-backup.json"`)
	respond(w, 200, b)
}

func (s *server) restore(w http.ResponseWriter, r *http.Request) {
	var b core.Backup
	if !decodeLimit(w, r, &b, 32<<20) {
		return
	}
	old, err := s.repo.Channels(r.Context())
	if err != nil {
		fail(w, 500, "无法读取频道")
		return
	}
	if err := s.repo.Import(r.Context(), b); err != nil {
		fail(w, 400, "备份格式或配置无效，导入失败")
		return
	}
	for _, ch := range old {
		s.media.Invalidate(ch.ID)
	}
	s.invalidateAll(r.Context())
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *server) password(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OldPassword string `json:"old_password"`
		Password    string `json:"password"`
		Confirm     string `json:"confirm"`
	}
	if !decode(w, r, &body) {
		return
	}
	if len(body.Password) < 12 || len(body.Password) > 72 {
		fail(w, 400, "新密码需要 12–72 字节（中文字符占多个字节）")
		return
	}
	if body.Password != body.Confirm {
		fail(w, 400, "两次输入的新密码不一致")
		return
	}
	if !s.allowLogin(r) {
		fail(w, 429, "密码验证尝试过多，请稍后重试")
		return
	}
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	hash, err := s.repo.AdminHash(r.Context())
	if err != nil || len(body.OldPassword) > 72 || bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.OldPassword)) != nil {
		fail(w, 403, "当前密码错误")
		return
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		fail(w, 500, "无法更新密码")
		return
	}
	if err := s.repo.SetAdminHash(r.Context(), string(newHash)); err != nil {
		fail(w, 500, "无法更新密码")
		return
	}
	s.mu.Lock()
	clear(s.sessions)
	s.mu.Unlock()
	s.cookie(w, "", -1)
	respond(w, 200, map[string]bool{"ok": true})
}

func (s *server) rotateToken(w http.ResponseWriter, r *http.Request) {
	settings, err := s.repo.Settings(r.Context())
	if err != nil {
		fail(w, 500, "无法读取设置")
		return
	}
	settings.PlaybackToken, err = token()
	if err != nil {
		fail(w, 500, "无法生成播放令牌")
		return
	}
	if err := s.repo.SaveSettings(r.Context(), settings); err != nil {
		fail(w, 500, "无法更新播放令牌")
		return
	}
	s.invalidateAll(r.Context())
	respond(w, 200, map[string]bool{"ok": true})
}
