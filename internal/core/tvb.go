package core

import (
	"net/url"
	"strings"
)

// TVBChannelID accepts only the two supported public TVB news pages.
func TVBChannelID(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "news.tvb.com") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return ""
	}
	parts := strings.Split(strings.TrimSuffix(u.Path, "/"), "/")
	if len(parts) != 4 || (parts[1] != "tc" && parts[1] != "sc" && parts[1] != "en") || parts[2] != "live" || (parts[3] != "C" && parts[3] != "F") {
		return ""
	}
	return parts[3]
}
