package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"iptv-manager/internal/core"
)

type memoryRepo struct {
	hash     string
	channels []core.Channel
	settings core.Settings
	changes  int
	bulkErr  error
}

func (r *memoryRepo) Channels(context.Context) ([]core.Channel, error) { return r.channels, nil }
func (r *memoryRepo) Channel(_ context.Context, id string) (core.Channel, error) {
	for _, ch := range r.channels {
		if ch.ID == id {
			return ch, nil
		}
	}
	return core.Channel{}, errors.New("not found")
}
func (r *memoryRepo) SaveChannel(_ context.Context, ch core.Channel) (core.Channel, error) {
	r.changes++
	if ch.ID == "" {
		ch.ID = "created"
	}
	return ch, nil
}
func (r *memoryRepo) AddChannels(context.Context, core.BulkChannelRequest) (core.BulkChannelResult, error) {
	r.changes++
	return core.BulkChannelResult{}, r.bulkErr
}
func (r *memoryRepo) Subscriptions(context.Context) ([]core.Subscription, error) {
	return []core.Subscription{}, nil
}
func (r *memoryRepo) SaveSubscription(_ context.Context, sub core.Subscription) (core.Subscription, error) {
	r.changes++
	return sub, nil
}
func (r *memoryRepo) DeleteSubscription(context.Context, string) error { r.changes++; return nil }
func (r *memoryRepo) ClearSubscription(context.Context, string) ([]string, error) {
	r.changes++
	return nil, nil
}
func (r *memoryRepo) SaveProxy(_ context.Context, p core.Proxy) (core.Proxy, error) {
	r.changes++
	if p.ID == "" {
		p.ID = "0123456789abcdef01234567"
	}
	return p, nil
}
func (r *memoryRepo) DeleteProxy(context.Context, string) error              { r.changes++; return nil }
func (r *memoryRepo) SetProviderProxy(context.Context, string, string) error { r.changes++; return nil }
func (r *memoryRepo) DeleteChannel(context.Context, string) error            { r.changes++; return nil }
func (r *memoryRepo) UpdateChannels(context.Context, core.BulkChannelUpdate) error {
	r.changes++
	return nil
}
func (r *memoryRepo) DeleteChannels(context.Context, []string) error  { r.changes++; return nil }
func (r *memoryRepo) Reorder(context.Context, []string) error         { r.changes++; return nil }
func (r *memoryRepo) Settings(context.Context) (core.Settings, error) { return r.settings, nil }
func (r *memoryRepo) SaveSettings(_ context.Context, s core.Settings) error {
	r.settings = s
	r.changes++
	return nil
}
func (r *memoryRepo) AdminHash(context.Context) (string, error) { return r.hash, nil }
func (r *memoryRepo) SetAdminHash(_ context.Context, h string) error {
	r.hash = h
	r.changes++
	return nil
}
func (r *memoryRepo) Export(context.Context) (core.Backup, error) {
	return core.Backup{Version: 4, Settings: r.settings, Channels: r.channels}, nil
}
func (r *memoryRepo) Import(context.Context, core.Backup) error { r.changes++; return nil }
func (r *memoryRepo) Traffic(_ context.Context, month string) (core.Traffic, error) {
	return core.Traffic{Month: month, Bytes: 123}, nil
}

type fakeMedia struct{ invalidated []string }

func (*fakeMedia) Statuses() map[string]core.ChannelStatus { return map[string]core.ChannelStatus{} }
func (m *fakeMedia) Invalidate(id string)                  { m.invalidated = append(m.invalidated, id) }

func setup(t *testing.T) (http.Handler, *memoryRepo, *fakeMedia) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("a-strong-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	repo := &memoryRepo{hash: string(hash), settings: core.Settings{PlaybackToken: "existing-token", DefaultQuality: 720, MonthlyBudgetGB: 800}, channels: []core.Channel{{ID: "test-channel", Name: "测试", Enabled: true}}}
	media := &fakeMedia{}
	handler, err := New(repo, media, Options{SecureCookies: true, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return handler, repo, media
}

func request(h http.Handler, method, path, body string, cookie *http.Cookie, csrf, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://tv.example"+path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.1:12345"
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func loginAsAdmin(t *testing.T, h http.Handler) (*http.Cookie, string) {
	t.Helper()
	w := request(h, "POST", "/api/login", `{"password":"a-strong-password"}`, nil, "", "https://tv.example")
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing session cookie")
	}
	c := cookies[0]
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("insecure cookie: %+v", c)
	}
	var payload struct {
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil || payload.CSRF == "" {
		t.Fatal("missing csrf")
	}
	return c, payload.CSRF
}

func TestUnauthenticatedMutationsNeverReachRepository(t *testing.T) {
	h, repo, media := setup(t)
	for _, route := range []struct{ method, path string }{{"POST", "/api/channels"}, {"POST", "/api/channels/bulk"}, {"POST", "/api/channels/bulk-update"}, {"POST", "/api/channels/bulk-delete"}, {"DELETE", "/api/channels/test-channel"}, {"PUT", "/api/settings"}, {"POST", "/api/restore"}, {"POST", "/api/token"}, {"POST", "/api/channels/test-channel/refresh"}} {
		w := request(h, route.method, route.path, `{}`, nil, "", "")
		if w.Code != 401 {
			t.Errorf("%s %s: %d", route.method, route.path, w.Code)
		}
	}
	if repo.changes != 0 || len(media.invalidated) != 0 {
		t.Fatal("unauthenticated operation changed data")
	}
}

func TestCSRFAndOrigin(t *testing.T) {
	h, repo, _ := setup(t)
	cookie, csrf := loginAsAdmin(t, h)
	for _, tc := range []struct{ csrf, origin string }{{"", "https://tv.example"}, {csrf, "https://evil.example"}, {csrf, "null"}, {csrf, "https://tv.example.evil.example"}} {
		w := request(h, "DELETE", "/api/channels/test-channel", "", cookie, tc.csrf, tc.origin)
		if w.Code != 403 {
			t.Errorf("expected csrf failure, got %d", w.Code)
		}
	}
	if repo.changes != 0 {
		t.Fatal("CSRF changed data")
	}
	w := request(h, "DELETE", "/api/channels/test-channel", "", cookie, csrf, "https://tv.example")
	if w.Code != 200 || repo.changes != 1 {
		t.Fatalf("legitimate operation failed: %d", w.Code)
	}
	w = request(h, "POST", "/api/login", `{"password":"a-strong-password"}`, nil, "", "https://evil.example")
	if w.Code != 403 {
		t.Fatal("cross-origin login accepted")
	}
}

func TestPasswordChangeValidatesAndRevokesEverySession(t *testing.T) {
	h, repo, _ := setup(t)
	cookie, csrf := loginAsAdmin(t, h)
	second, _ := loginAsAdmin(t, h)
	for _, payload := range []string{
		`{"old_password":"wrong","password":"a-new-password-long","confirm":"a-new-password-long"}`,
		`{"old_password":"a-strong-password","password":"short","confirm":"short"}`,
		`{"old_password":"a-strong-password","password":"a-new-password-long","confirm":"different-password"}`,
		`{"old_password":"a-strong-password","password":"` + strings.Repeat("a", 73) + `","confirm":"` + strings.Repeat("a", 73) + `"}`,
	} {
		w := request(h, "POST", "/api/password", payload, cookie, csrf, "https://tv.example")
		if w.Code < 400 {
			t.Fatalf("invalid password change accepted: %d", w.Code)
		}
	}
	if repo.changes != 0 {
		t.Fatal("invalid password persisted")
	}
	w := request(h, "POST", "/api/password", `{"old_password":"a-strong-password","password":"a-new-password-long","confirm":"a-new-password-long"}`, cookie, csrf, "https://tv.example")
	if w.Code != 200 {
		t.Fatalf("password change: %d %s", w.Code, w.Body.String())
	}
	if bcrypt.CompareHashAndPassword([]byte(repo.hash), []byte("a-new-password-long")) != nil {
		t.Fatal("new password not hashed")
	}
	for _, c := range []*http.Cookie{cookie, second} {
		if request(h, "GET", "/api/state", "", c, "", "").Code != 401 {
			t.Fatal("old session survived password change")
		}
	}
}

func TestLoginRateLimitCannotBeBypassedWithForwardedFor(t *testing.T) {
	h, _, _ := setup(t)
	for i := 0; i < 11; i++ {
		r := httptest.NewRequest("POST", "https://tv.example/api/login", strings.NewReader(`{"password":"wrong"}`))
		r.RemoteAddr = "192.0.2.1:12345"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Forwarded-For", string(rune('a'+i)))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if i < 10 && w.Code != 401 {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
		if i == 10 && w.Code != 429 {
			t.Fatalf("rate limit bypassed: %d", w.Code)
		}
	}
}

func TestSettingsCannotOverwritePlaybackToken(t *testing.T) {
	h, repo, media := setup(t)
	cookie, csrf := loginAsAdmin(t, h)
	w := request(h, "PUT", "/api/settings", `{"default_quality":480,"playback_token":"attacker-chosen"}`, cookie, csrf, "")
	if w.Code != 200 {
		t.Fatalf("settings: %d %s", w.Code, w.Body.String())
	}
	if repo.settings.PlaybackToken != "existing-token" {
		t.Fatal("settings silently changed playback token")
	}
	if len(media.invalidated) != 1 || media.invalidated[0] != "test-channel" {
		t.Fatal("settings did not invalidate media")
	}
}

func TestStrictJSONAndChannelID(t *testing.T) {
	h, repo, _ := setup(t)
	cookie, csrf := loginAsAdmin(t, h)
	for _, body := range []string{`{"unknown":"field"}`, `{} {}`, `{"name":"` + strings.Repeat("x", maxBody) + `"}`} {
		w := request(h, "POST", "/api/channels", body, cookie, csrf, "")
		if w.Code != 400 {
			t.Fatalf("invalid body accepted: %d", w.Code)
		}
	}
	if repo.changes != 0 {
		t.Fatal("invalid body changed repo")
	}
	w := request(h, "PUT", "/api/channels/test-channel", `{"id":"replacement-id","name":"changed"}`, cookie, csrf, "")
	var saved core.Channel
	_ = json.Unmarshal(w.Body.Bytes(), &saved)
	if w.Code != 200 || saved.ID != "test-channel" {
		t.Fatal("client changed permanent channel ID")
	}
}

func TestStaticUIAndLogout(t *testing.T) {
	h, _, _ := setup(t)
	for _, path := range []string{"/", "/style.css", "/app.js"} {
		w := request(h, "GET", path, "", nil, "", "")
		if w.Code != 200 || w.Header().Get("Content-Security-Policy") == "" {
			t.Fatalf("asset %s: %d", path, w.Code)
		}
	}
	cookie, csrf := loginAsAdmin(t, h)
	w := request(h, "POST", "/api/logout", "", cookie, csrf, "")
	if w.Code != 200 {
		t.Fatal("logout failed")
	}
	if request(h, "GET", "/api/state", "", cookie, "", "").Code != 401 {
		t.Fatal("logged-out session still accepted")
	}
}
