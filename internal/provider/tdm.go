package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"iptv-manager/internal/core"
)

// TDM publishes one catalog URL per channel but serves it from different CDN
// hosts depending on where the viewer connects from. The official player asks
// get-domain and rewrites the URL before playback; this resolver does the same
// through the proxy configured for the source, so the host matches the egress.
const tdmDomainAPI = "https://www.tdm.com.mo/api/v1/common/get-domain"
const tdmDomainTTL = time.Hour
const tdmFailureTTL = 30 * time.Second

func tdmDefinition() definition {
	return definition{catalog: Catalog{
		ID:           "tdm",
		Name:         "澳视澳门",
		Website:      "https://www.tdm.com.mo/zh-hant/live?Channel=1&type=tv",
		Description:  "来自澳广视（TDM）官网的电视直播地址。播放时按官网规则把直播域名换成当前出口可用的 CDN。默认勾选 91 至 96 台，CGTN 和会议直播可按需添加。",
		SourceType:   "builtin",
		DefaultMode:  "relay",
		LinkLabel:    "原始 M3U8 ↗",
		PlaybackHelp: "支持直连或中继，原始画质由播放器选择。直播域名在每次播放时通过官网接口确定，与来源的出站代理一致。",
		Channels: []Channel{
			{ID: "ctvp", Name: "澳視澳門 91台", URL: "https://live3.tdm.com.mo/ch1/ch1.live/playlist.m3u8", Selected: true, Note: ""},
			{ID: "ptvp", Name: "澳視葡文 92台", URL: "https://live3.tdm.com.mo/ch2/ch2.live/playlist.m3u8", Selected: true, Note: ""},
			{ID: "info", Name: "澳門資訊 94台", URL: "https://live3.tdm.com.mo/ch5/info_ch5.live/playlist.m3u8", Selected: true, Note: ""},
			{ID: "sports", Name: "澳門體育 93台", URL: "https://live3.tdm.com.mo/ch4/sport_ch4.live/playlist.m3u8", Selected: true, Note: ""},
			{ID: "hdctvp", Name: "澳門綜藝 95台", URL: "https://live3.tdm.com.mo/ch6/hd_ch6.live/playlist.m3u8", Selected: true, Note: ""},
			{ID: "satellite", Name: "澳門-Macau 96台", URL: "https://live3.tdm.com.mo/ch3/ch3.live/playlist.m3u8", Selected: true, Note: ""},
			{ID: "cgtn71", Name: "CCTV 綜合頻道 71台", URL: "https://live3.tdm.com.mo/cgtn/cgtn71/playlist.m3u8", Selected: false, Note: ""},
			{ID: "cgtn", Name: "CGTN 73台", URL: "https://live3.tdm.com.mo/cgtn/cgtn73/playlist.m3u8", Selected: false, Note: ""},
			{ID: "cgtn74", Name: "CGTN 紀錄頻道 74台", URL: "https://live3.tdm.com.mo/cgtn/cgtn74/playlist.m3u8", Selected: false, Note: ""},
			{ID: "legislativec", Name: "立法會直播", URL: "https://live3.tdm.com.mo/tv/ch21.live/playlist.m3u8", Selected: false, Note: "会议直播，节目安排以官网为准"},
			{ID: "legislativep", Name: "Directo das Reuniões da Assembleia de Macau", URL: "https://live3.tdm.com.mo/tv/ch22.live/playlist.m3u8", Selected: false, Note: "会议直播，节目安排以官网为准"},
		},
	},
		create: func(opts Options) resolver { return newTDMResolver(opts.ClientFactory, opts.Now) },
	}
}

// tdmDomains is the rewrite table returned by get-domain. Replacements keep
// the response order because the official player applies them sequentially.
type tdmDomains struct {
	source, live string
	replacements [][2]string
	ok           bool
	expires      time.Time
}

type tdmResolver struct {
	client func(string) (*http.Client, error)
	now    func() time.Time
	mu     sync.Mutex
	cache  map[string]tdmDomains // keyed by the egress (proxy URL, "" for direct)
}

func newTDMResolver(factory func(string) (*http.Client, error), now func() time.Time) *tdmResolver {
	if factory == nil {
		factory = newTDMClient
	}
	if now == nil {
		now = time.Now
	}
	return &tdmResolver{client: factory, now: now, cache: make(map[string]tdmDomains)}
}

func newTDMClient(proxy string) (*http.Client, error) {
	return newProviderClient(proxy, validateTDMURL)
}

func validateTDMURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") || len(raw) > 4096 {
		return errors.New("TDM 资源地址无效")
	}
	host := strings.ToLower(u.Hostname())
	if host != "tdm.com.mo" && !strings.HasSuffix(host, ".tdm.com.mo") {
		return errors.New("TDM 资源必须属于 tdm.com.mo 域名")
	}
	return nil
}

func (m *tdmResolver) Resolve(ctx context.Context, ch core.Channel, settings core.Settings, _ string) (Playback, error) {
	proxy := core.EffectiveProxy(ch, settings)
	table := m.domains(ctx, proxy)
	if !table.ok {
		return Playback{URL: ch.URL}, nil
	}
	rewritten, err := applyTDMDomains(ch.URL, table)
	if err != nil {
		slog.Warn("TDM 域名改写结果无效，使用目录地址", "channel", ch.ID, "reason", err.Error())
		return Playback{URL: ch.URL}, nil
	}
	return Playback{URL: rewritten}, nil
}

// Invalidate drops the cached table so the next play asks the API again.
func (m *tdmResolver) Invalidate(string) {
	m.mu.Lock()
	clear(m.cache)
	m.mu.Unlock()
}

func (m *tdmResolver) domains(ctx context.Context, proxy string) tdmDomains {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cached, found := m.cache[proxy]; found && m.now().Before(cached.expires) {
		return cached
	}
	table, err := m.fetch(ctx, proxy)
	mode := "direct"
	if proxy != "" {
		mode = "proxy"
	}
	if err != nil {
		// Keep today's catalog address and retry shortly; do not hammer the API.
		slog.Warn("TDM 域名接口不可用，使用目录地址", "proxy_mode", mode, "reason", err.Error())
		table = tdmDomains{expires: m.now().Add(tdmFailureTTL)}
	} else {
		slog.Info("TDM 域名表已更新", "proxy_mode", mode, "live_domain", table.live, "replacements", len(table.replacements))
		table.ok = true
		table.expires = m.now().Add(tdmDomainTTL)
	}
	m.cache[proxy] = table
	return table
}

func (m *tdmResolver) fetch(ctx context.Context, proxy string) (tdmDomains, error) {
	client, err := m.client(proxy)
	if err != nil {
		return tdmDomains{}, errors.New("无法创建连接")
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tdmDomainAPI, nil)
	if err != nil {
		return tdmDomains{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return tdmDomains{}, errors.New(providerNetworkReason(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return tdmDomains{}, errors.New("接口返回 HTTP " + http.StatusText(resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return tdmDomains{}, errors.New("读取接口响应失败")
	}
	return parseTDMDomains(body)
}

func parseTDMDomains(body []byte) (tdmDomains, error) {
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Domains          json.RawMessage `json:"domains"`
			LiveDomain       string          `json:"liveDomain"`
			SourceLiveDomain string          `json:"sourceLiveDomain"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Code != 0 {
		return tdmDomains{}, errors.New("接口响应格式无效")
	}
	table := tdmDomains{source: payload.Data.SourceLiveDomain, live: payload.Data.LiveDomain}
	for _, origin := range []string{table.source, table.live} {
		if origin != "" && validateTDMURL(origin) != nil {
			return tdmDomains{}, errors.New("接口返回了不允许的直播域名")
		}
	}
	if len(payload.Data.Domains) > 0 && string(payload.Data.Domains) != "null" {
		dec := json.NewDecoder(bytes.NewReader(payload.Data.Domains))
		if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
			return tdmDomains{}, errors.New("接口响应格式无效")
		}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return tdmDomains{}, errors.New("接口响应格式无效")
			}
			var value string
			if err := dec.Decode(&value); err != nil {
				return tdmDomains{}, errors.New("接口响应格式无效")
			}
			from, _ := key.(string)
			if validateTDMURL(from) != nil || validateTDMURL(value) != nil {
				return tdmDomains{}, errors.New("接口返回了不允许的直播域名")
			}
			if len(table.replacements) >= 20 {
				return tdmDomains{}, errors.New("接口返回的域名表过大")
			}
			table.replacements = append(table.replacements, [2]string{from, value})
		}
	}
	return table, nil
}

// applyTDMDomains mirrors the official player: swap the source live domain for
// the regional one, then apply each table entry once in order.
func applyTDMDomains(raw string, table tdmDomains) (string, error) {
	out := raw
	if table.source != "" && table.live != "" && strings.Contains(out, table.source) {
		out = strings.Replace(out, table.source, table.live, 1)
	}
	for _, pair := range table.replacements {
		out = strings.Replace(out, pair[0], pair[1], 1)
	}
	if err := validateTDMURL(out); err != nil {
		return "", err
	}
	return out, nil
}
