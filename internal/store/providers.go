package store

import (
	"iptv-manager/internal/core"
	"iptv-manager/internal/provider"
)

func parseBuiltinChannels(req core.BulkChannelRequest) (core.BulkChannelResult, []core.Channel, error) {
	catalog, ok := provider.Lookup(req.ProviderID)
	if !ok || req.SourceType != "builtin" || req.Text != "" || len(req.ChannelIDs) == 0 || len(req.ChannelIDs) > maxBulkChannels {
		return core.BulkChannelResult{}, nil, invalid("请选择有效的网站直播源和频道")
	}
	if req.Group == "" {
		req.Group = catalog.Name
	}
	if req.Mode == "" {
		req.Mode = catalog.DefaultMode
	}
	result := core.BulkChannelResult{Results: make([]core.BulkChannelItem, 0, len(req.ChannelIDs))}
	channels := make([]core.Channel, 0, len(req.ChannelIDs))
	for i, key := range req.ChannelIDs {
		item, ok := provider.ChannelInfo(req.ProviderID, key)
		if !ok {
			return core.BulkChannelResult{}, nil, invalid("网站来源频道不存在")
		}
		// The proxy of built-in channels is configured per provider, not per channel.
		ch, err := normalizeChannel(core.Channel{SourceType: "builtin", ProviderID: req.ProviderID, ProviderChannelID: key, Name: item.Name, URL: item.URL, Group: req.Group, Mode: req.Mode, Enabled: true, Proxy: core.DirectProxy})
		if err != nil {
			return core.BulkChannelResult{}, nil, err
		}
		channels = append(channels, ch)
		result.Results = append(result.Results, core.BulkChannelItem{Line: i + 1, Name: ch.Name, URL: ch.URL})
	}
	return result, channels, nil
}

func channelIdentity(ch core.Channel) string {
	if ch.IsBuiltin() {
		return "builtin\x00" + ch.ProviderID + "\x00" + ch.ProviderChannelID
	}
	return ch.SourceType + "\x00" + ch.URL
}
