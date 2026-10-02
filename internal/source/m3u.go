package source

import (
	"bufio"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

const MaxPlaylistBytes = 2 << 20
const MaxPlaylistChannels = 1000

type Entry struct{ Key, Name, URL, Group, Logo string }
type Playlist struct {
	Entries []Entry
	Skipped int
}

var attributes = regexp.MustCompile(`([A-Za-z0-9_-]+)="([^"\r\n]*)"`)

func cleanText(raw string, limit int) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			continue
		}
		if r == '"' {
			r = '\''
		}
		if b.Len()+len(string(r)) > limit {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ParseM3U reads an IPTV channel list, not an HLS rendition/segment playlist.
func ParseM3U(body []byte, base *url.URL) (Playlist, error) {
	var result Playlist
	if len(body) > MaxPlaylistBytes {
		return result, errors.New("M3U 列表不能超过 2 MB")
	}
	text := strings.TrimSpace(strings.TrimPrefix(string(body), "\uFEFF"))
	if text != "#EXTM3U" && !strings.HasPrefix(text, "#EXTM3U\n") && !strings.HasPrefix(text, "#EXTM3U\r\n") && !strings.HasPrefix(text, "#EXTM3U ") {
		return result, errors.New("来源没有返回有效的 M3U 频道列表")
	}
	seen := map[string]string{}
	pending := Entry{}
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 4096), 64<<10)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-") {
			return Playlist{}, errors.New("此地址是单个 HLS 直播，请在添加频道中选择通用直播源")
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			pending = Entry{}
			quoted := false
			for i, r := range line {
				if r == '"' {
					quoted = !quoted
				}
				if r == ',' && !quoted {
					pending.Name = cleanText(line[i+1:], 300)
					break
				}
			}
			for _, a := range attributes.FindAllStringSubmatch(line, -1) {
				switch a[1] {
				case "tvg-id":
					pending.Key = cleanText(a[2], 300)
				case "tvg-name":
					if pending.Name == "" {
						pending.Name = cleanText(a[2], 300)
					}
				case "group-title":
					pending.Group = cleanText(a[2], 150)
				case "tvg-logo":
					pending.Logo = a[2]
				}
			}
			continue
		}
		if strings.HasPrefix(line, "#EXTGRP:") {
			pending.Group = cleanText(strings.TrimPrefix(line, "#EXTGRP:"), 150)
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		entry := pending
		pending = Entry{}
		u, err := url.Parse(line)
		if err != nil {
			result.Skipped++
			continue
		}
		if base != nil {
			u = base.ResolveReference(u)
		}
		entry.URL = u.String()
		if ValidateURL(entry.URL) != nil {
			result.Skipped++
			continue
		}
		if entry.Name == "" {
			entry.Name = cleanText(u.Hostname()+u.Path, 300)
		}
		if entry.Logo != "" {
			logo, err := url.Parse(entry.Logo)
			if err == nil && base != nil {
				logo = base.ResolveReference(logo)
			}
			if err != nil || ValidateURL(logo.String()) != nil || len(logo.String()) > 2048 {
				entry.Logo = ""
			} else {
				entry.Logo = logo.String()
			}
		}
		if entry.Key != "" {
			entry.Key = "id:" + entry.Key
		} else {
			entry.Key = "url:" + entry.URL
		}
		if previous, ok := seen[entry.Key]; ok {
			if previous != entry.URL {
				return Playlist{}, errors.New("M3U 中存在重复 tvg-id 且地址不同，无法确定频道身份")
			}
			result.Skipped++
			continue
		}
		seen[entry.Key] = entry.URL
		result.Entries = append(result.Entries, entry)
		if len(result.Entries) > MaxPlaylistChannels {
			return Playlist{}, errors.New("M3U 列表最多支持 1000 个频道")
		}
	}
	if scanner.Err() != nil {
		return Playlist{}, errors.New("M3U 内容读取失败或单行过长")
	}
	if len(result.Entries) == 0 {
		return Playlist{}, errors.New("M3U 列表没有可用的 HTTP/HTTPS 频道，现有频道已保留")
	}
	return result, nil
}
