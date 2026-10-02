package media

import (
	"bufio"
	"errors"
	"log/slog"
	"math"
	"net/url"
	"strconv"
	"strings"

	"iptv-manager/internal/resolver"
)

var errMasterSelection = errors.New("HLS 主清单中找不到与选中格式一致的视频，需要重新解析来源")

type masterVariant struct {
	line, uri string
	url       string
	attrs     map[string]string
}

type masterSelectionInfo struct {
	Match                             string
	Variants, Candidates, AudioTracks int
}

// YouTube puts itag and id in path parameters, or occasionally in the query.
// Reject duplicates rather than resolving conflicting rendition identities.
func manifestParameter(u *url.URL, key string) (string, bool) {
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", false
	}
	all := values[key]
	parts := strings.Split(u.Path, "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == key {
			all = append(all, parts[i+1])
		}
	}
	if len(all) == 0 {
		return "", true
	}
	if len(all) != 1 || all[0] == "" {
		return "", false
	}
	return all[0], true
}

func renditionIdentity(raw string) (itag, content string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", false
	}
	itag, ok = manifestParameter(u, "itag")
	if !ok || len(itag) == 0 || len(itag) > 6 || strings.Trim(itag, "0123456789") != "" {
		return "", "", false
	}
	content, ok = manifestParameter(u, "id")
	return itag, content, ok
}

func matchesVideoMetadata(attrs map[string]string, height int, format resolver.VideoFormat) bool {
	rawWidth, rawHeight, ok := strings.Cut(strings.ToLower(attrs["RESOLUTION"]), "x")
	w, werr := strconv.Atoi(rawWidth)
	h, herr := strconv.Atoi(rawHeight)
	if !ok || werr != nil || herr != nil || w <= 0 || height <= 0 || h != height || (format.Width > 0 && w != format.Width) {
		return false
	}
	if format.Codec != "" {
		matched := false
		for _, codec := range strings.Split(attrs["CODECS"], ",") {
			if strings.EqualFold(strings.TrimSpace(codec), format.Codec) {
				matched = true
			}
		}
		if !matched {
			return false
		}
	}
	if format.FPS > 0 {
		fps, err := strconv.ParseFloat(attrs["FRAME-RATE"], 64)
		if err != nil || math.IsNaN(fps) || math.IsInf(fps, 0) || math.Abs(fps-format.FPS) > 0.05 {
			return false
		}
	}
	return true
}

// Keep the actual upstream association. Audio codec metadata in yt-dlp's JSON
// may be absent even when EXT-X-MEDIA supplies a valid external audio track.
// Limiting the master itself prevents clients from choosing a higher quality.
func selectMasterVariant(body []byte, base *url.URL, videoURL string, height int, format resolver.VideoFormat) ([]byte, masterSelectionInfo, error) {
	info := masterSelectionInfo{Match: "none"}
	text := strings.TrimPrefix(string(body), "\ufeff")
	if !strings.HasPrefix(strings.TrimSpace(text), "#EXTM3U") {
		return nil, info, errors.New("独立音轨需要有效的 HLS 主清单")
	}
	var globals, media []string
	var selected *masterVariant
	var variants []masterVariant
	var pending string
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 4096), 512*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			if pending != "" {
				return nil, info, errors.New("HLS 主清单的视频地址缺失")
			}
			pending = line
		case strings.HasPrefix(line, "#EXT-X-MEDIA:"):
			media = append(media, line)
		case strings.HasPrefix(line, "#EXTINF:"), strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			return nil, info, errors.New("独立音轨来源返回了媒体清单，需要 HLS 主清单")
		case !strings.HasPrefix(line, "#"):
			if pending == "" {
				return nil, info, errors.New("HLS 主清单的视频条目不完整")
			}
			u, err := url.Parse(line)
			if err != nil || strings.Contains(line, "{$") {
				return nil, info, errors.New("HLS 主清单的视频地址无效")
			}
			_, attrs, _ := strings.Cut(pending, ":")
			variants = append(variants, masterVariant{line: pending, uri: line, url: base.ResolveReference(u).String(), attrs: parseAttributes(attrs)})
			pending = ""
		case line == "#EXTM3U", line == "#EXT-X-INDEPENDENT-SEGMENTS",
			strings.HasPrefix(line, "#EXT-X-VERSION:"), strings.HasPrefix(line, "#EXT-X-START:"),
			strings.HasPrefix(line, "#EXT-X-SESSION-KEY:"), strings.HasPrefix(line, "#EXT-X-SESSION-DATA:"):
			globals = append(globals, line)
		}
	}
	if scanner.Err() != nil || pending != "" || len(globals) == 0 || globals[0] != "#EXTM3U" {
		return nil, info, errors.New("HLS 主清单行过长或视频条目不完整")
	}
	info.Variants = len(variants)
	for i := range variants {
		if variants[i].url == videoURL {
			selected = &variants[i]
			info.Candidates++
			info.Match = "exact_url"
		}
	}
	// Select only within this video's original master. Signed URLs may change
	// between yt-dlp's fetch and ours; itag, resolution and codec must still
	// identify exactly one rendition. Never fall back to height alone, and use
	// the current master's URI and AUDIO group rather than an old signed URL.
	if selected == nil && format.Codec != "" {
		itag, content, valid := renditionIdentity(videoURL)
		if valid {
			for i := range variants {
				v := &variants[i]
				candidateItag, candidateContent, candidateValid := renditionIdentity(v.url)
				if candidateValid && itag == candidateItag && content == candidateContent && matchesVideoMetadata(v.attrs, height, format) {
					selected = v
					info.Candidates++
					info.Match = "youtube_itag"
				}
			}
		}
	}
	if info.Candidates > 1 {
		return nil, info, errors.New("HLS 主清单包含多个匹配视频，无法确认音轨关联")
	}
	if selected == nil {
		return nil, info, errMasterSelection
	}
	if !matchesVideoMetadata(selected.attrs, height, format) {
		return nil, info, errMasterSelection
	}
	if selected.attrs["VIDEO"] != "" {
		return nil, info, errors.New("暂不支持含替代视频组的 HLS 主清单")
	}
	group := selected.attrs["AUDIO"]
	if group == "" {
		return nil, info, errors.New("选中的 HLS 视频没有关联音轨，不能作为有声直播播放")
	}
	var tracks []string
	audioCount := 0
	for _, line := range media {
		_, attributes, _ := strings.Cut(line, ":")
		attrs := parseAttributes(attributes)
		kind := attrs["TYPE"]
		if kind != "AUDIO" && kind != "SUBTITLES" && kind != "CLOSED-CAPTIONS" {
			continue
		}
		if attrs["GROUP-ID"] == "" || attrs["GROUP-ID"] != selected.attrs[kind] {
			continue
		}
		if kind == "AUDIO" {
			if attrs["URI"] == "" || attrs["NAME"] == "" {
				return nil, info, errors.New("HLS 音轨组缺少外部音轨地址或名称")
			}
			audioCount++
		}
		tracks = append(tracks, line)
	}
	if audioCount == 0 {
		return nil, info, errors.New("HLS 主清单缺少视频所关联的音轨组")
	}
	lines := append(globals, tracks...)
	lines = append(lines, selected.line, selected.uri)
	info.AudioTracks = audioCount
	return []byte(strings.Join(lines, "\n") + "\n"), info, nil
}

func (s *Server) prepareManifest(body []byte, base *url.URL, ref resource) ([]byte, error) {
	if ref.VideoURL == "" || ref.URL != ref.RootURL {
		return body, nil
	}
	if err := s.options.ValidateURL(ref.VideoURL); err != nil {
		return nil, errors.New("选中的 HLS 视频地址不被允许")
	}
	filtered, info, err := selectMasterVariant(body, base, ref.VideoURL, ref.Height, ref.VideoFormat)
	if err == nil {
		filtered, err = RewriteHLS(filtered, base, func(raw string) (string, error) {
			if err := s.options.ValidateURL(raw); err != nil {
				return "", errors.New("HLS 主清单包含不允许的音轨或资源地址")
			}
			return raw, nil
		})
	}
	if err != nil {
		itag, _, _ := renditionIdentity(ref.VideoURL)
		slog.Warn("HLS 音轨关联验证失败", "channel", ref.Channel, "error", err.Error(), "selected_itag", itag, "height", ref.Height, "variants", info.Variants, "candidates", info.Candidates, "match", info.Match, "refreshable", errors.Is(err, errMasterSelection))
		s.reportFailure(ref.Channel, err.Error())
		return nil, err
	}
	slog.Info("HLS 音视频关联成功", "channel", ref.Channel, "height", ref.Height, "audio_tracks", info.AudioTracks, "match", info.Match, "variants", info.Variants)
	return filtered, nil
}
