package store

import (
	"context"
	"database/sql"

	"iptv-manager/internal/core"
	"iptv-manager/internal/provider"
)

// Schema 2 -> 3 is atomic. Only exact, registered legacy sources are tagged;
// IDs, URLs, order, playback settings and subscription ownership are preserved.
func migrateProviders(ctx context.Context, tx *sql.Tx) error {
	for _, stmt := range []string{
		`ALTER TABLE channels ADD COLUMN provider_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE channels ADD COLUMN provider_channel_id TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+channelFields+` FROM channels`)
	if err != nil {
		return err
	}
	var changes []core.Channel
	for rows.Next() {
		ch, err := scanChannel(rows)
		if err != nil {
			rows.Close()
			return err
		}
		upgraded := provider.UpgradeLegacy(ch)
		if !upgraded.IsBuiltin() && !upgraded.IsStream() && upgraded.SourceType != "youtube" {
			rows.Close()
			return invalid("无法识别旧内置来源，数据库未更新")
		}
		if upgraded.ProviderID != ch.ProviderID {
			changes = append(changes, upgraded)
		}
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	for _, ch := range changes {
		if _, err := tx.ExecContext(ctx, `UPDATE channels SET source_type=?,provider_id=?,provider_channel_id=? WHERE id=?`, ch.SourceType, ch.ProviderID, ch.ProviderChannelID, ch.ID); err != nil {
			return err
		}
	}
	return nil
}

func parseBuiltinChannels(req core.BulkChannelRequest) (core.BulkChannelResult, []core.Channel, error) {
	catalog, ok := provider.Lookup(req.ProviderID)
	if !ok || req.SourceType != "builtin" || req.Text != "" || len(req.ChannelIDs) == 0 || len(req.ChannelIDs) > maxBulkChannels {
		return core.BulkChannelResult{}, nil, invalid("请选择有效的内置直播源和频道")
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
			return core.BulkChannelResult{}, nil, invalid("内置频道不存在")
		}
		ch, err := normalizeChannel(core.Channel{SourceType: "builtin", ProviderID: req.ProviderID, ProviderChannelID: key, Name: item.Name, URL: item.URL, Group: req.Group, Mode: req.Mode, Enabled: true, Proxy: "inherit"})
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
