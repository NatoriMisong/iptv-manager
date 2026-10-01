package media

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

var uriAttribute = regexp.MustCompile(`\b(?:URI|SERVER-URI)="([^"\r\n]*)"`)

// playlistLink identifies a rendition by its HLS attributes, never its position
// or media sequence. A refreshed master may reorder variants or advance segments.
type playlistLink struct {
	URL      string
	Playlist bool
	Selector string
}

// RewriteHLS resolves every resource against the final response URL, including
// separate audio, encryption keys, initialization maps and nested playlists.
func RewriteHLS(body []byte, base *url.URL, rewrite func(string) (string, error)) ([]byte, error) {
	return rewriteHLSLinks(body, base, func(link playlistLink) (string, error) { return rewrite(link.URL) })
}

func rewriteHLSLinks(body []byte, base *url.URL, rewrite func(playlistLink) (string, error)) ([]byte, error) {
	text := strings.TrimPrefix(string(body), "\ufeff")
	if !strings.HasPrefix(strings.TrimSpace(text), "#EXTM3U") {
		return nil, errors.New("上游没有返回有效的 HLS 清单")
	}
	resolve := func(raw string, playlist bool, selector string) (string, error) {
		u, err := url.Parse(raw)
		if err != nil || raw == "" || strings.Contains(raw, "{$") {
			return "", errors.New("暂不支持此 HLS 资源地址")
		}
		return rewrite(playlistLink{URL: base.ResolveReference(u).String(), Playlist: playlist, Selector: selector})
	}
	var out strings.Builder
	var pendingVariant string
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 4096), 512*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			out.WriteByte('\n')
			continue
		}
		if !strings.HasPrefix(line, "#") {
			mapped, err := resolve(line, pendingVariant != "", pendingVariant)
			if err != nil {
				return nil, err
			}
			out.WriteString(mapped)
			pendingVariant = ""
		} else {
			if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
				pendingVariant = playlistSelector(line)
			}
			isPlaylist := strings.HasPrefix(line, "#EXT-X-MEDIA:") || strings.HasPrefix(line, "#EXT-X-I-FRAME-STREAM-INF:") || strings.HasPrefix(line, "#EXT-X-RENDITION-REPORT:")
			selector := ""
			if isPlaylist && !strings.HasPrefix(line, "#EXT-X-RENDITION-REPORT:") {
				selector = playlistSelector(line)
			}
			var rewriteErr error
			line = uriAttribute.ReplaceAllStringFunc(line, func(attr string) string {
				match := uriAttribute.FindStringSubmatch(attr)
				mapped, err := resolve(match[1], isPlaylist, selector)
				if err != nil {
					rewriteErr = err
					return attr
				}
				return attr[:strings.IndexByte(attr, '"')+1] + mapped + "\""
			})
			if rewriteErr != nil {
				return nil, rewriteErr
			}
			out.WriteString(line)
		}
		out.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.New("HLS 清单行过长")
	}
	return []byte(out.String()), nil
}

func playlistSelector(line string) string {
	tag, text, ok := strings.Cut(line, ":")
	if !ok {
		return ""
	}
	attrs := parseAttributes(text)
	// Bandwidth is included to disambiguate streams with the same dimensions.
	// Exact matching deliberately fails if an upstream changes the rendition.
	keys := []string{"BANDWIDTH", "RESOLUTION", "CODECS", "FRAME-RATE", "AUDIO", "VIDEO", "SUBTITLES", "CLOSED-CAPTIONS", "VIDEO-RANGE"}
	if tag == "#EXT-X-MEDIA" {
		keys = []string{"TYPE", "GROUP-ID", "NAME", "LANGUAGE", "ASSOC-LANGUAGE", "CHANNELS"}
	}
	identity := map[string]string{"tag": tag}
	for _, key := range keys {
		if value, ok := attrs[key]; ok {
			identity[key] = value
		}
	}
	if len(identity) == 1 {
		return ""
	}
	encoded, _ := json.Marshal(identity)
	return string(encoded)
}

func parseAttributes(text string) map[string]string {
	attrs := make(map[string]string)
	for len(text) > 0 {
		text = strings.TrimLeft(text, " ,\t")
		key, rest, ok := strings.Cut(text, "=")
		if !ok {
			break
		}
		key = strings.TrimSpace(key)
		if strings.HasPrefix(rest, `"`) {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				break
			}
			attrs[key] = rest[1 : end+1]
			text = rest[end+2:]
		} else {
			value, next, found := strings.Cut(rest, ",")
			attrs[key] = strings.TrimSpace(value)
			if !found {
				break
			}
			text = next
		}
	}
	return attrs
}
