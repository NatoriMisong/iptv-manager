package resolver

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"unicode"
)

func (r *Resolver) logFor(k cacheKey) *slog.Logger {
	video := "unknown"
	if u, err := url.Parse(k.source); err == nil {
		id := u.Query().Get("v")
		if u.Hostname() == "youtu.be" {
			id = strings.TrimPrefix(u.Path, "/")
		}
		if videoIDPattern.MatchString(id) {
			video = id
		}
	}
	return r.logger.With("channel", k.id, "video", video)
}

func proxyMode(raw string) string {
	if raw == "" {
		return "direct"
	}
	if u, err := url.Parse(raw); err == nil {
		return u.Scheme
	}
	return "configured"
}

func commandExitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

func commandErrorKind(err error) string {
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, os.ErrNotExist):
		return "executable_not_found"
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	default:
		return "process_failed"
	}
}

var (
	videoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	ansiPattern    = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	urlPattern     = regexp.MustCompile(`(?i)\b(?:https?|socks5h?)://[^\s<>"']+`)
	headerPattern  = regexp.MustCompile(`(?im)\b(?:set-cookie|cookie|proxy-authorization|authorization)\s*[:=][^\r\n]*`)
	optionPattern  = regexp.MustCompile(`(?i)--(?:cookies|username|password|proxy|add-headers?)\b[=\s]+(?:"[^"]*"|'[^']*'|[^\s,]+)`)
	secretPattern  = regexp.MustCompile(`(?i)\b(?:access_token|refresh_token|token|signature|sig|lsig|sparams|api[_-]?key|password|passwd)\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
	labelPattern   = regexp.MustCompile(`^[A-Za-z0-9_.+\-]{1,80}$`)
)

func diagnosticSecrets(proxy, cookies string) []string {
	secrets := []string{proxy, cookies}
	if u, err := url.Parse(proxy); err == nil && u.User != nil {
		password, _ := u.User.Password()
		secrets = append(secrets, u.User.Username(), password, u.User.String())
	}
	return secrets
}

// Scrub the entire bounded stderr before truncating it, so a cut-off URL or
// header cannot leave a partial secret in logs. Raw stdout is never logged.
func sanitizeDiagnostic(raw string, secrets ...string) string {
	text := ansiPattern.ReplaceAllString(raw, "")
	text = headerPattern.ReplaceAllString(text, "[redacted header]")
	text = urlPattern.ReplaceAllString(text, "[redacted URL]")
	text = optionPattern.ReplaceAllString(text, "[redacted option]")
	text = secretPattern.ReplaceAllString(text, "[redacted credential]")
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, text)
	const limit = 8192
	runes := []rune(strings.TrimSpace(text))
	if len(runes) > limit {
		return string(runes[:limit]) + " …[truncated]"
	}
	return string(runes)
}

func diagnosticLabel(value string) string {
	if value == "" {
		return "missing"
	}
	if !labelPattern.MatchString(value) {
		return "unrecognized"
	}
	return value
}

// These observations explain the existing selection rules; they do not
// participate in selection and never fetch, merge, or transcode media.
func rejectionReason(f format, quality int) string {
	switch {
	case f.Protocol != "m3u8" && f.Protocol != "m3u8_native":
		return "not_hls"
	case !validMediaURL(f.URL):
		return "invalid_media_url"
	case !knownCodec(f.VCodec):
		return "missing_video_codec"
	case f.Height <= 0:
		return "unknown_height"
	case f.Height > quality:
		return "above_quality_limit"
	case !knownCodec(f.ACodec) && (!validMediaURL(f.ManifestURL) || f.URL == f.ManifestURL):
		return "missing_audio_master"
	default:
		return "eligible"
	}
}

func logFormatDiagnostics(logger *slog.Logger, data []byte, quality int) {
	var info videoInfo
	if json.Unmarshal(data, &info) != nil {
		return
	}
	formats := info.Formats
	if info.URL != "" {
		found := false
		for _, f := range formats {
			if f.URL == info.URL && f.Protocol == info.Protocol {
				found = true
				break
			}
		}
		if !found {
			formats = append(formats, info.format)
		}
	}
	counts := make(map[string]int)
	hls := 0
	for _, f := range formats {
		counts[rejectionReason(f, quality)]++
		if f.Protocol == "m3u8" || f.Protocol == "m3u8_native" {
			hls++
		}
	}
	logger.Warn("HLS 格式筛选结果", "quality_limit", quality, "formats_total", len(formats), "hls_total", hls, "rejections", counts)
	shown := 0
	for _, f := range formats {
		if hls > 0 && f.Protocol != "m3u8" && f.Protocol != "m3u8_native" {
			continue
		}
		if shown >= 20 {
			break
		}
		logger.Warn("格式未入选", "format_id", diagnosticLabel(f.ID), "protocol", diagnosticLabel(f.Protocol), "height", f.Height, "vcodec", diagnosticLabel(f.VCodec), "acodec", diagnosticLabel(f.ACodec), "reason", rejectionReason(f, quality))
		shown++
	}
}
