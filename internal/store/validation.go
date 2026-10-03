package store

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"iptv-manager/internal/core"
	"iptv-manager/internal/provider"
	"iptv-manager/internal/source"
)

var (
	videoIDPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	channelIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	tokenPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{32,256}$`)
	proxyIDPattern   = regexp.MustCompile(`^[0-9a-f]{24}$`)
	monthPattern     = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])$`)
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrValidation, fmt.Sprintf(format, args...))
}

func safeText(value string, maximum int) bool {
	if len(value) > maximum {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || r == '"' || r == '\u2028' || r == '\u2029' {
			return false
		}
	}
	return true
}

func youtubeURL(raw string) (string, error) {
	if !safeText(raw, 2048) {
		return "", invalid("YouTube URL contains unsupported characters")
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Port() != "" || !validPort(u) {
		return "", invalid("a YouTube watch URL is required")
	}
	var id string
	switch strings.ToLower(u.Hostname()) {
	case "youtube.com", "www.youtube.com", "m.youtube.com":
		if u.Path != "/watch" {
			return "", invalid("YouTube URL must point to /watch")
		}
		query, err := url.ParseQuery(u.RawQuery)
		if err != nil || len(query["v"]) != 1 {
			return "", invalid("YouTube URL requires one video ID")
		}
		id = query.Get("v")
	case "youtu.be", "www.youtu.be":
		id = strings.TrimPrefix(u.Path, "/")
	default:
		return "", invalid("only youtube.com and youtu.be sources are supported")
	}
	if !videoIDPattern.MatchString(id) {
		return "", invalid("YouTube video ID must contain 11 letters, digits, underscores or hyphens")
	}
	return "https://www.youtube.com/watch?v=" + id, nil
}

func validQuality(value int, inherit bool) bool {
	if value == 0 && inherit {
		return true
	}
	switch value {
	case 144, 240, 360, 480, 720, 1080, 1440, 2160:
		return true
	default:
		return false
	}
}

func validPort(u *url.URL) bool {
	if strings.HasSuffix(u.Host, ":") {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		return err == nil && n > 0 && n <= 65535
	}
	return true
}

const maxProxies = 50

// proxyRef accepts only "direct" or the ID of a saved proxy. Free-form proxy
// URLs are configured once on the proxy page, never on channels.
func proxyRef(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == core.DirectProxy {
		return core.DirectProxy, nil
	}
	if proxyIDPattern.MatchString(raw) {
		return raw, nil
	}
	return "", invalid("proxy must be direct or the ID of a saved proxy")
}

func credentialText(value string) bool {
	if len(value) > 256 {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func normalizeProxy(p core.Proxy) (core.Proxy, error) {
	if p.ID != "" && !proxyIDPattern.MatchString(p.ID) {
		return p, invalid("invalid proxy ID")
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || !safeText(p.Name, 80) {
		return p, invalid("proxy name must be 1–80 bytes without quotes or control characters")
	}
	p.Scheme = strings.ToLower(strings.TrimSpace(p.Scheme))
	if p.Scheme != "http" && p.Scheme != "https" && p.Scheme != "socks5" {
		return p, invalid("proxy scheme must be http, https or socks5")
	}
	p.Host = strings.ToLower(strings.Trim(strings.TrimSpace(p.Host), "[]"))
	if p.Host == "" || len(p.Host) > 253 || strings.ContainsAny(p.Host, " \t\r\n/\\@#?\"'<>%") {
		return p, invalid("invalid proxy host")
	}
	if p.Port < 1 || p.Port > 65535 {
		return p, invalid("proxy port must be between 1 and 65535")
	}
	if !credentialText(p.Username) || !credentialText(p.Password) {
		return p, invalid("proxy credentials cannot exceed 256 bytes or contain control characters")
	}
	u, err := url.Parse(p.URL())
	if err != nil || u.Hostname() != p.Host || u.Port() != strconv.Itoa(p.Port) {
		return p, invalid("invalid proxy host")
	}
	return p, nil
}

func normalizeChannel(ch core.Channel) (core.Channel, error) {
	if ch.ID != "" && !channelIDPattern.MatchString(ch.ID) {
		return ch, invalid("invalid channel ID")
	}
	if !safeText(ch.Name, 300) || !safeText(ch.Group, 150) {
		return ch, invalid("name and group cannot contain quotes, control characters or excessive text")
	}
	ch.Name = strings.TrimSpace(ch.Name)
	ch.Group = strings.TrimSpace(ch.Group)
	if ch.Name == "" {
		return ch, invalid("channel name cannot be empty")
	}
	var err error
	if ch.SourceType == "" {
		ch.SourceType = "youtube"
	}
	if ch.IsBuiltin() {
		ch, err = provider.Normalize(ch)
	} else {
		if ch.ProviderID != "" || ch.ProviderChannelID != "" {
			return ch, invalid("普通来源不能带网站来源标识")
		}
		ch.URL, err = channelURL(ch.SourceType, ch.URL)
	}
	if err != nil {
		return ch, invalid("%s", err)
	}
	if ch.Logo != "" {
		u, err := url.Parse(ch.Logo)
		if !safeText(ch.Logo, 2048) || err != nil || u == nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || !validPort(u) {
			return ch, invalid("logo must be an HTTP(S) URL without credentials or control characters")
		}
	}
	if ch.Mode == "" {
		ch.Mode = "relay"
	}
	if ch.Mode != "relay" && ch.Mode != "direct" {
		return ch, invalid("channel mode must be relay or direct")
	}
	if !validQuality(ch.Quality, true) {
		return ch, invalid("unsupported channel quality")
	}
	if ch.IsStream() || ch.IsBuiltin() {
		ch.Quality = 0
	}
	if ch.SortOrder < 0 {
		return ch, invalid("sort order cannot be negative")
	}
	if ch.IsBuiltin() {
		// Built-in channels share the provider-level proxy setting.
		ch.Proxy = core.DirectProxy
		return ch, nil
	}
	ch.Proxy, err = proxyRef(ch.Proxy)
	return ch, err
}

func channelURL(kind, raw string) (string, error) {
	if kind == "" || kind == "youtube" {
		return youtubeURL(raw)
	}
	if kind != "stream" {
		return "", invalid("unsupported source type")
	}
	if err := source.ValidateURL(raw); err != nil {
		return "", invalid("%s", err)
	}
	u, _ := url.Parse(raw)
	u.Host = strings.ToLower(u.Host)
	return u.String(), nil
}

func normalizeSettings(s core.Settings) (core.Settings, error) {
	if s.BaseURL != "" {
		if !safeText(s.BaseURL, 2048) {
			return s, invalid("base URL contains unsupported characters")
		}
		u, err := url.Parse(s.BaseURL)
		if err != nil || u == nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || !validPort(u) {
			return s, invalid("base URL must be an HTTP(S) origin, for example http://tv.example.com:9000")
		}
		u.Path = ""
		s.BaseURL = u.String()
	}
	if !validQuality(s.DefaultQuality, false) {
		return s, invalid("unsupported default quality")
	}
	if s.MonthlyBudgetGB < 1 || s.MonthlyBudgetGB > 1000000 {
		return s, invalid("monthly budget must be between 1 and 1000000 GB")
	}
	if !tokenPattern.MatchString(s.PlaybackToken) {
		return s, invalid("playback token requires 32–256 URL-safe letters, digits, underscores or hyphens")
	}
	s = completeSettings(s)
	if len(s.Proxies) > maxProxies {
		return s, invalid("at most %d proxies can be saved", maxProxies)
	}
	names := make(map[string]bool, len(s.Proxies))
	ids := make(map[string]bool, len(s.Proxies))
	for i, p := range s.Proxies {
		p, err := normalizeProxy(p)
		if err != nil {
			return s, err
		}
		if p.ID == "" || ids[p.ID] {
			return s, invalid("proxy IDs must be present and unique")
		}
		if names[strings.ToLower(p.Name)] {
			return s, invalid("proxy names must be unique")
		}
		ids[p.ID], names[strings.ToLower(p.Name)] = true, true
		s.Proxies[i] = p
	}
	for id, raw := range s.ProviderProxies {
		ref, err := proxyRef(raw)
		if err != nil {
			return s, err
		}
		if err := checkProxyRef(s, ref); err != nil {
			return s, invalid("built-in source %s references a missing proxy", id)
		}
		s.ProviderProxies[id] = ref
	}
	return s, nil
}
