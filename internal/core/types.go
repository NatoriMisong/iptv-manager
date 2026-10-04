package core

import (
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DirectProxy is the proxy reference meaning "connect without a proxy".
const DirectProxy = "direct"

// Channel IDs are permanent; SortOrder is independent of the playback URL.
type Channel struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	URL               string `json:"url"`
	SourceType        string `json:"source_type"` // youtube (default), stream, builtin
	ProviderID        string `json:"provider_id,omitempty"`
	ProviderChannelID string `json:"provider_channel_id,omitempty"`
	SubscriptionID    string `json:"subscription_id,omitempty"`
	SourceID          string `json:"source_id,omitempty"`    // list inside the subscription
	SourceKey         string `json:"source_key,omitempty"`   // channel name in that list
	SourceGroup       string `json:"source_group,omitempty"` // group-title from the list, informational
	SourceMissing     bool   `json:"source_missing,omitempty"`
	Group             string `json:"group"`
	Logo              string `json:"logo"`
	Enabled           bool   `json:"enabled"`
	SortOrder         int    `json:"sort_order"`
	Mode              string `json:"mode"`    // relay (default) or direct
	Quality           int    `json:"quality"` // 0 inherits settings
	Proxy             string `json:"proxy"`   // direct or a saved proxy ID; built-in channels use the provider setting
}

// BulkChannelRequest adds channels from one URL or "name,URL" per line.
// New channels are enabled and use the requested proxy (direct by default).
type BulkChannelRequest struct {
	ProviderID string   `json:"provider_id,omitempty"`
	ChannelIDs []string `json:"channel_ids,omitempty"`
	SourceType string   `json:"source_type"`
	Text       string   `json:"text"`
	Group      string   `json:"group"`
	Mode       string   `json:"mode"`
	Quality    int      `json:"quality"`
	Proxy      string   `json:"proxy,omitempty"`
}

// BulkChannelUpdate edits the user-managed fields of several channels in one
// transaction. Nil fields stay unchanged. Subscription channels keep the group
// synced from their list and built-in channels keep the provider proxy, exactly
// as when editing a single channel.
type BulkChannelUpdate struct {
	IDs     []string `json:"ids"`
	Group   *string  `json:"group,omitempty"`
	Mode    *string  `json:"mode,omitempty"`
	Quality *int     `json:"quality,omitempty"`
	Proxy   *string  `json:"proxy,omitempty"`
	Enabled *bool    `json:"enabled,omitempty"`
}

// Subscription is a catalogue of channels fetched from one or more M3U lists.
// Syncing refreshes the catalogue and the URLs of channels the user already
// picked from it; it never adds channels on its own.
type Subscription struct {
	ID              string               `json:"id"`
	Name            string               `json:"name"`
	Sources         []SubscriptionSource `json:"sources"`
	Proxy           string               `json:"proxy"` // direct or a saved proxy ID, used when fetching the lists
	IntervalMinutes int                  `json:"interval_minutes"`
	Enabled         bool                 `json:"enabled"`
	Defaults        []DomainDefault      `json:"defaults,omitempty"` // playback defaults per channel URL domain
	Revision        int                  `json:"revision"`
	LastAttempt     time.Time            `json:"last_attempt"`
	LastSync        time.Time            `json:"last_sync"` // last run where every list succeeded
	LastError       string               `json:"last_error"`
}

// SubscriptionSource is one M3U list URL with its own sync status. The ID is
// stable for as long as the URL text stays the same.
type SubscriptionSource struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	LastSync  time.Time `json:"last_sync"`
	LastError string    `json:"last_error"`
	Entries   int       `json:"entries"`
	Skipped   int       `json:"skipped"`
}

// HasSource reports whether the subscription contains a list with this ID.
func (sub Subscription) HasSource(id string) bool {
	for _, src := range sub.Sources {
		if src.ID == id {
			return true
		}
	}
	return false
}

// DomainDefault is the playback mode and proxy applied to channels whose URL
// host belongs to Domain (registrable domain such as demo.org, or an IP) when
// they are added from a list.
type DomainDefault struct {
	Domain string `json:"domain"`
	Mode   string `json:"mode"`
	Proxy  string `json:"proxy"`
}

// SiteDomain reduces a host to its registrable domain: a.demo.org and
// b.demo.org both give demo.org, news.bbc.co.uk gives bbc.co.uk, IP
// addresses are returned unchanged. It is a heuristic, not a public suffix list.
func SiteDomain(host string) string {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" || net.ParseIP(host) != nil {
		return host
	}
	labels := strings.Split(host, ".")
	if len(labels) <= 2 {
		return host
	}
	n := 2
	switch labels[len(labels)-2] {
	case "com", "net", "org", "edu", "gov", "mil", "co", "ac", "or", "ne", "go", "idv":
		if len(labels[len(labels)-1]) == 2 {
			n = 3
		}
	}
	return strings.Join(labels[len(labels)-n:], ".")
}

// URLDomain returns SiteDomain of a URL's host, or "" when it cannot be parsed.
func URLDomain(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return SiteDomain(u.Hostname())
}

// SubscriptionEntry is one catalogue row; Added reports whether the user has
// already created a channel from it.
type SubscriptionEntry struct {
	SourceID  string `json:"source_id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Logo      string `json:"logo,omitempty"`
	Group     string `json:"group,omitempty"`
	Position  int    `json:"position"`
	Added     bool   `json:"added,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
}

// SubscriptionAddRequest creates channels from catalogue entries. Group, when
// set, overrides the list group for every new channel; Defaults are matched by
// URL domain and remembered on the subscription for next time.
type SubscriptionAddRequest struct {
	Items    []SubscriptionAddItem `json:"items"`
	Group    string                `json:"group"`
	Defaults []DomainDefault       `json:"defaults"`
}
type SubscriptionAddItem struct {
	SourceID string `json:"source_id"`
	Name     string `json:"name"`
}

func (ch Channel) IsStream() bool  { return ch.SourceType == "stream" }
func (ch Channel) IsBuiltin() bool { return ch.SourceType == "builtin" }

type BulkChannelItem struct {
	Line      int    `json:"line"`
	Name      string `json:"name"`
	URL       string `json:"url,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	Status    string `json:"status"` // added, skipped, failed
	Message   string `json:"message"`
}

type BulkChannelResult struct {
	Added   int               `json:"added"`
	Skipped int               `json:"skipped"`
	Failed  int               `json:"failed"`
	Results []BulkChannelItem `json:"results"`
}

// Proxy is a named outbound proxy. Channels, subscriptions and built-in
// providers reference it by ID so the address is edited in one place.
type Proxy struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Scheme   string `json:"scheme"` // http, https, socks5
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// URL renders the client URL. Credentials are percent-encoded, so passwords
// containing "@" or ":" need no manual escaping.
func (p Proxy) URL() string {
	host := p.Host
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	u := &url.URL{Scheme: p.Scheme, Host: host + ":" + strconv.Itoa(p.Port)}
	if p.Username != "" || p.Password != "" {
		u.User = url.UserPassword(p.Username, p.Password)
	}
	return u.String()
}

// Address is the display form without credentials.
func (p Proxy) Address() string {
	return Proxy{Scheme: p.Scheme, Host: p.Host, Port: p.Port}.URL()
}

type Settings struct {
	BaseURL         string            `json:"base_url"`
	DefaultQuality  int               `json:"default_quality"`
	MonthlyBudgetGB int               `json:"monthly_budget_gb"`
	PlaybackToken   string            `json:"playback_token"`
	Proxies         []Proxy           `json:"proxies"`
	ProviderProxies map[string]string `json:"provider_proxies"` // provider ID -> direct or proxy ID
}

func (s Settings) FindProxy(id string) (Proxy, bool) {
	for _, p := range s.Proxies {
		if p.ID == id {
			return p, true
		}
	}
	return Proxy{}, false
}

// ProxyURL resolves a reference to a client URL; "" means direct. A dangling
// reference yields an unusable URL so every client fails closed instead of
// silently connecting without the configured proxy.
func (s Settings) ProxyURL(ref string) string {
	if ref == "" || ref == DirectProxy {
		return ""
	}
	if p, ok := s.FindProxy(ref); ok {
		return p.URL()
	}
	return "invalid://missing-proxy"
}

// ProxyRef returns the reference a channel uses: built-in channels share the
// provider setting, every other channel has its own.
func ProxyRef(ch Channel, settings Settings) string {
	if ch.IsBuiltin() {
		return settings.ProviderProxies[ch.ProviderID]
	}
	return ch.Proxy
}

func EffectiveProxy(ch Channel, settings Settings) string {
	return settings.ProxyURL(ProxyRef(ch, settings))
}

type Backup struct {
	Version       int            `json:"version"`
	Settings      Settings       `json:"settings"`
	Channels      []Channel      `json:"channels"`
	Subscriptions []Subscription `json:"subscriptions,omitempty"`
}

type ChannelStatus struct {
	State     string    `json:"state"`
	Message   string    `json:"message"`
	Title     string    `json:"title,omitempty"`
	Height    int       `json:"height,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

type Traffic struct {
	Month string `json:"month"`
	Bytes int64  `json:"bytes"`
}
