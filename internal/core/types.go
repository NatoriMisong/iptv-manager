package core

import "time"

// Channel IDs are permanent; SortOrder is independent of the playback URL.
type Channel struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Group     string `json:"group"`
	Logo      string `json:"logo"`
	Enabled   bool   `json:"enabled"`
	SortOrder int    `json:"sort_order"`
	Mode      string `json:"mode"`    // inherit, relay, direct
	Quality   int    `json:"quality"` // 0 inherits settings
	Proxy     string `json:"proxy"`   // inherit, direct, or an HTTP(S)/SOCKS5 URL
}

// BulkChannelRequest adds channels from one URL or "name,URL" per line.
// New channels are enabled and inherit the global proxy.
type BulkChannelRequest struct {
	Text    string `json:"text"`
	Group   string `json:"group"`
	Mode    string `json:"mode"`
	Quality int    `json:"quality"`
}

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

type Settings struct {
	BaseURL         string `json:"base_url"`
	DefaultMode     string `json:"default_mode"`
	DefaultQuality  int    `json:"default_quality"`
	UpstreamProxy   string `json:"upstream_proxy"`
	MonthlyBudgetGB int    `json:"monthly_budget_gb"`
	PlaybackToken   string `json:"playback_token"`
}

type Backup struct {
	Version  int       `json:"version"`
	Settings Settings  `json:"settings"`
	Channels []Channel `json:"channels"`
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

func EffectiveProxy(ch Channel, settings Settings) string {
	p := ch.Proxy
	if p == "" || p == "inherit" {
		p = settings.UpstreamProxy
	}
	if p == "direct" || p == "inherit" {
		return ""
	}
	return p
}
