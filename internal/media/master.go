package media

import (
	"bufio"
	"errors"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
)

type masterVariant struct {
	line, uri string
	attrs     map[string]string
}

// Keep the actual upstream association. Audio codec metadata in yt-dlp's JSON
// may be absent even when EXT-X-MEDIA supplies a valid external audio track.
// Limiting the master itself prevents clients from choosing a higher quality.
func selectMasterVariant(body []byte, base *url.URL, videoURL string, height int) ([]byte, int, error) {
	text := strings.TrimPrefix(string(body), "\ufeff")
	if !strings.HasPrefix(strings.TrimSpace(text), "#EXTM3U") {
		return nil, 0, errors.New("独立音轨需要有效的 HLS 主清单")
	}
	var globals, media []string
	var selected *masterVariant
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
				return nil, 0, errors.New("HLS 主清单的视频地址缺失")
			}
			pending = line
		case strings.HasPrefix(line, "#EXT-X-MEDIA:"):
			media = append(media, line)
		case strings.HasPrefix(line, "#EXTINF:"), strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			return nil, 0, errors.New("独立音轨来源返回了媒体清单，需要 HLS 主清单")
		case !strings.HasPrefix(line, "#"):
			if pending == "" {
				return nil, 0, errors.New("HLS 主清单的视频条目不完整")
			}
			u, err := url.Parse(line)
			if err != nil || strings.Contains(line, "{$") {
				return nil, 0, errors.New("HLS 主清单的视频地址无效")
			}
			if base.ResolveReference(u).String() == videoURL {
				if selected != nil {
					return nil, 0, errors.New("HLS 主清单包含多个匹配视频，无法确认音轨关联")
				}
				_, attrs, _ := strings.Cut(pending, ":")
				selected = &masterVariant{line: pending, uri: line, attrs: parseAttributes(attrs)}
			}
			pending = ""
		case line == "#EXTM3U", line == "#EXT-X-INDEPENDENT-SEGMENTS",
			strings.HasPrefix(line, "#EXT-X-VERSION:"), strings.HasPrefix(line, "#EXT-X-START:"),
			strings.HasPrefix(line, "#EXT-X-SESSION-KEY:"), strings.HasPrefix(line, "#EXT-X-SESSION-DATA:"):
			globals = append(globals, line)
		}
	}
	if scanner.Err() != nil || pending != "" || len(globals) == 0 || globals[0] != "#EXTM3U" {
		return nil, 0, errors.New("HLS 主清单行过长或视频条目不完整")
	}
	if selected == nil {
		return nil, 0, errors.New("HLS 主清单中找不到选中的视频，请刷新来源")
	}
	_, rawHeight, ok := strings.Cut(strings.ToLower(selected.attrs["RESOLUTION"]), "x")
	actualHeight, err := strconv.Atoi(rawHeight)
	if !ok || err != nil || height <= 0 || actualHeight != height {
		return nil, 0, errors.New("HLS 主清单的分辨率与选中视频不一致，请刷新来源")
	}
	if selected.attrs["VIDEO"] != "" {
		return nil, 0, errors.New("暂不支持含替代视频组的 HLS 主清单")
	}
	group := selected.attrs["AUDIO"]
	if group == "" {
		return nil, 0, errors.New("选中的 HLS 视频没有关联音轨，不能作为有声直播播放")
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
				return nil, 0, errors.New("HLS 音轨组缺少外部音轨地址或名称")
			}
			audioCount++
		}
		tracks = append(tracks, line)
	}
	if audioCount == 0 {
		return nil, 0, errors.New("HLS 主清单缺少视频所关联的音轨组")
	}
	lines := append(globals, tracks...)
	lines = append(lines, selected.line, selected.uri)
	return []byte(strings.Join(lines, "\n") + "\n"), audioCount, nil
}

func (s *Server) prepareManifest(body []byte, base *url.URL, ref resource) ([]byte, error) {
	if ref.VideoURL == "" || ref.URL != ref.RootURL {
		return body, nil
	}
	if err := s.options.ValidateURL(ref.VideoURL); err != nil {
		return nil, errors.New("选中的 HLS 视频地址不被允许")
	}
	filtered, tracks, err := selectMasterVariant(body, base, ref.VideoURL, ref.Height)
	if err == nil {
		filtered, err = RewriteHLS(filtered, base, func(raw string) (string, error) {
			if err := s.options.ValidateURL(raw); err != nil {
				return "", errors.New("HLS 主清单包含不允许的音轨或资源地址")
			}
			return raw, nil
		})
	}
	if err != nil {
		slog.Warn("HLS 音轨关联验证失败", "channel", ref.Channel, "error", err.Error())
		s.reportFailure(ref.Channel, err.Error())
		return nil, err
	}
	slog.Info("HLS 音视频关联成功", "channel", ref.Channel, "height", ref.Height, "audio_tracks", tracks)
	return filtered, nil
}
