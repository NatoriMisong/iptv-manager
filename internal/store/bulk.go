package store

import (
	"context"
	"net/url"
	"strings"
	"unicode/utf8"

	"iptv-manager/internal/core"
	"iptv-manager/internal/provider"
)

const maxBulkChannels = 100
const maxBulkText = 256 << 10

// AddChannels skips invalid and duplicate rows, and commits all valid additions
// together. A database error rolls back the batch and returns no success result.
func (s *Store) AddChannels(ctx context.Context, req core.BulkChannelRequest) (core.BulkChannelResult, error) {
	result, channels, err := parseBulkChannels(req)
	if err != nil {
		return core.BulkChannelResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.BulkChannelResult{}, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+channelFields+` FROM channels ORDER BY sort_order,id`)
	if err != nil {
		return core.BulkChannelResult{}, err
	}
	seen := make(map[string]string)
	nextOrder := 0
	for rows.Next() {
		ch, err := scanChannel(rows)
		if err != nil {
			rows.Close()
			return core.BulkChannelResult{}, err
		}
		for _, key := range []string{channelIdentity(ch), channelIdentity(provider.UpgradeLegacy(ch))} {
			if seen[key] == "" {
				seen[key] = ch.ID
			}
		}
		if ch.SortOrder >= nextOrder {
			nextOrder = ch.SortOrder + 1
		}
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return core.BulkChannelResult{}, err
	}
	if closeErr != nil {
		return core.BulkChannelResult{}, closeErr
	}
	for i, ch := range channels {
		item := &result.Results[i]
		if item.Status == "failed" {
			continue
		}
		id := seen[channelIdentity(ch)]
		if id == "" {
			id = seen[channelIdentity(provider.UpgradeLegacy(ch))]
		}
		if id != "" {
			item.Status, item.Message, item.ChannelID = "skipped", "此链接已存在，已跳过", id
			result.Skipped++
			continue
		}
		ch.ID, err = randomHex(12)
		if err != nil {
			return core.BulkChannelResult{}, err
		}
		ch.SortOrder = nextOrder
		if err := checkChannelCapacity(ctx, tx, 1); err != nil {
			return core.BulkChannelResult{}, err
		}
		if err := insertChannel(ctx, tx, ch); err != nil {
			return core.BulkChannelResult{}, err
		}
		nextOrder++
		seen[channelIdentity(ch)] = ch.ID
		item.Status, item.Message, item.ChannelID = "added", "已添加", ch.ID
		result.Added++
	}
	if err := tx.Commit(); err != nil {
		return core.BulkChannelResult{}, err
	}
	return result, nil
}

func parseBulkChannels(req core.BulkChannelRequest) (core.BulkChannelResult, []core.Channel, error) {
	if req.SourceType == "builtin" || req.ProviderID != "" || len(req.ChannelIDs) > 0 {
		return parseBuiltinChannels(req)
	}
	if req.SourceType == "" {
		req.SourceType = "youtube"
	}
	if req.SourceType != "youtube" && req.SourceType != "stream" {
		return core.BulkChannelResult{}, nil, invalid("来源类型无效")
	}
	if len(req.Text) > maxBulkText {
		return core.BulkChannelResult{}, nil, invalid("批量内容不能超过 256 KB")
	}
	if !safeText(req.Group, 150) {
		return core.BulkChannelResult{}, nil, invalid("分组不能超过 150 字节，或包含双引号、控制字符")
	}
	if req.Mode == "" {
		req.Mode = "inherit"
	}
	if req.Mode != "inherit" && req.Mode != "relay" && req.Mode != "direct" {
		return core.BulkChannelResult{}, nil, invalid("播放方式必须为继承全局、服务器中继或客户端直连")
	}
	if !validQuality(req.Quality, true) {
		return core.BulkChannelResult{}, nil, invalid("不支持此画质上限")
	}
	result := core.BulkChannelResult{Results: make([]core.BulkChannelItem, 0)}
	channels := make([]core.Channel, 0)
	content := strings.TrimPrefix(req.Text, "\uFEFF")
	content = strings.ReplaceAll(content, "\r\n", "\n")
	for line, raw := range strings.Split(content, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if len(result.Results) == maxBulkChannels {
			return core.BulkChannelResult{}, nil, invalid("每批最多添加 100 行频道，请分批提交")
		}
		ch := core.Channel{URL: raw, SourceType: req.SourceType, Group: req.Group, Mode: req.Mode, Quality: req.Quality, Enabled: true, Proxy: "inherit"}
		named := false
		// Do not split commas inside a bare URL's query string.
		if !strings.HasPrefix(raw, "https://") && !strings.HasPrefix(raw, "http://") {
			if split := strings.IndexAny(raw, ",，\t"); split >= 0 {
				_, size := utf8.DecodeRuneInString(raw[split:])
				ch.Name, ch.URL, named = strings.TrimSpace(raw[:split]), strings.TrimSpace(raw[split+size:]), true
			}
		}
		item := core.BulkChannelItem{Line: line + 1, Name: ch.Name}
		canonical, err := channelURL(ch.SourceType, ch.URL)
		if err != nil {
			item.Status, item.Message = "failed", "链接无效，请使用 YouTube watch?v=… 或 youtu.be/… 链接（视频 ID 为 11 位）"
			if ch.IsStream() {
				item.Message = "链接无效，请使用公网 HTTP/HTTPS 直播地址"
			}
		} else {
			ch.URL = canonical
			if !named {
				ch.Name = "YouTube " + strings.TrimPrefix(canonical, "https://www.youtube.com/watch?v=")
				if ch.IsStream() {
					u, _ := url.Parse(canonical)
					ch.Name = u.Hostname()
				}
			}
			item.Name, item.URL = ch.Name, ch.URL
			ch, err = normalizeChannel(ch)
			if err != nil {
				item.Status, item.Message = "failed", "频道名称不能为空、超过 300 字节，或包含双引号、控制字符"
			}
		}
		if item.Status == "failed" {
			result.Failed++
		}
		result.Results = append(result.Results, item)
		channels = append(channels, ch)
	}
	if len(result.Results) == 0 {
		return core.BulkChannelResult{}, nil, invalid("请至少填写一行直播链接")
	}
	return result, channels, nil
}
