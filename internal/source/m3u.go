package source

import (
	"bufio"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

const MaxPlaylistBytes = 4 << 20
const MaxPlaylistChannels = 5000

// Entry is one channel of a list. Name is the channel identity inside the
// list: URLs change between refreshes, names do not. Duplicate names keep the
// first occurrence.
type Entry struct{ Name, URL, Group, Logo string }
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

// resolveLink keeps absolute addresses exactly as written so that characters
// such as "|" or a trailing "#.m3u8" survive; only relative references are
// resolved against the list URL.
func resolveLink(raw string, u *url.URL, base *url.URL) string {
	if u.IsAbs() || base == nil {
		return raw
	}
	return base.ResolveReference(u).String()
}

// ParseM3U reads an IPTV channel list, not an HLS rendition/segment playlist.
func ParseM3U(body []byte, base *url.URL) (Playlist, error) {
	var result Playlist
	if len(body) > MaxPlaylistBytes {
		return result, errors.New("M3U 列表不能超过 4 MB")
	}
	text := strings.TrimSpace(strings.TrimPrefix(string(body), "\uFEFF"))
	if text != "#EXTM3U" && !strings.HasPrefix(text, "#EXTM3U\n") && !strings.HasPrefix(text, "#EXTM3U\r\n") && !strings.HasPrefix(text, "#EXTM3U ") {
		return result, errors.New("来源没有返回有效的 M3U 频道列表")
	}
	seen := map[string]bool{}
	pending := Entry{}
	tvgName := ""
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
			pending, tvgName = Entry{}, ""
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
				case "tvg-name":
					tvgName = cleanText(a[2], 300)
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
		if entry.Name == "" {
			entry.Name = tvgName
		}
		tvgName = ""
		u, err := url.Parse(line)
		if err != nil {
			result.Skipped++
			continue
		}
		entry.URL = resolveLink(line, u, base)
		if ValidateURL(entry.URL) != nil {
			result.Skipped++
			continue
		}
		entry.URL = Normalize(entry.URL)
		if entry.Name == "" || seen[entry.Name] {
			result.Skipped++
			continue
		}
		seen[entry.Name] = true
		if entry.Logo != "" {
			logo, err := url.Parse(entry.Logo)
			if err != nil {
				entry.Logo = ""
			} else {
				entry.Logo = resolveLink(entry.Logo, logo, base)
				if ValidateURL(entry.Logo) != nil || len(entry.Logo) > 2048 {
					entry.Logo = ""
				}
			}
		}
		result.Entries = append(result.Entries, entry)
		if len(result.Entries) > MaxPlaylistChannels {
			return Playlist{}, errors.New("M3U 列表最多支持 5000 个频道")
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
