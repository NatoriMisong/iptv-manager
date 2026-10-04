package source

import (
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestM3UParsing(t *testing.T) {
	base, _ := url.Parse("https://provider.example/lists/channels.m3u?token=private")
	body := "\ufeff#EXTM3U x-tvg-url=\"https://epg.example/feed.xml\"\r\n#EXTINF:-1 tvg-id=\"news\" tvg-logo=\"../logo.png\" group-title=\"新闻,直播\",频道一\r\n../live/index.m3u8?token=a%2Bb\r\n#EXTINF:-1,频道二\n#EXTGRP:体育\nhttps://media.example/live.ts\n#EXTINF:-1,不支持\nudp://239.1.1.1:1234\n#EXTINF:-1,私网\nhttp://127.0.0.1/stream\n#EXTINF:-1 tvg-logo=\"https://CDN.example/img/台标|1.png\",片段\nHTTPS://Media.Example/pxx.php?shk_cid=hdgd01#.m3u8\n#EXTINF:-1,\nhttps://media.example/live/{台}|a^b.m3u8?u=x|y\n"
	list, err := ParseM3U([]byte(body), base)
	if err != nil || len(list.Entries) != 4 || list.Skipped != 2 {
		t.Fatalf("parse: %+v %v", list, err)
	}
	if e := list.Entries[2]; e.URL != "https://media.example/pxx.php?shk_cid=hdgd01#.m3u8" || e.Key != "url:"+e.URL || e.Logo != "https://CDN.example/img/台标|1.png" {
		t.Fatalf("fragment and verbatim URL: %+v", e)
	}
	if e := list.Entries[3]; e.URL != "https://media.example/live/{台}|a^b.m3u8?u=x|y" || e.Name != "media.example/live/{台}|a^b.m3u8" {
		t.Fatalf("special characters re-encoded: %+v", e)
	}
	first := list.Entries[0]
	if first.Name != "频道一" || first.Key != "id:news" || first.Group != "新闻,直播" || first.URL != "https://provider.example/live/index.m3u8?token=a%2Bb" || first.Logo != "https://provider.example/logo.png" {
		t.Fatalf("metadata: %+v", first)
	}
	if list.Entries[1].Group != "体育" || list.Entries[1].Key != "url:https://media.example/live.ts" {
		t.Fatalf("fallback: %+v", list.Entries[1])
	}
	for _, bad := range []string{"<html>login</html>", "#EXTM3U\n", "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nsegment.ts", "#EXTM3U\n#EXTINF:-1 tvg-id=\"x\",A\nhttps://a.example/a\n#EXTINF:-1 tvg-id=\"x\",B\nhttps://a.example/b", strings.Repeat("a", MaxPlaylistBytes+1)} {
		if _, err := ParseM3U([]byte(bad), base); err == nil {
			t.Fatal("invalid playlist accepted")
		}
	}
}
func TestPublicHTTPValidation(t *testing.T) {
	for _, raw := range []string{"https://cdn.example/live.m3u8?token=a%2Bb", "http://203.0.113.10:8080/live.ts", "https://cdn.example/pxx.php?shk_cid=hdgd01#.m3u8", "https://cdn.example/a|b{c}.m3u8"} {
		if err := ValidateURL(raw); err != nil {
			t.Errorf("valid URL: %v", err)
		}
	}
	for _, raw := range []string{"file:///etc/passwd", "ftp://media.example/a", "http://user:secret@media.example/a", "http://localhost/a", "http://127.0.0.1/a", "http://[::1]/a", "http://169.254.169.254/metadata", "http://10.0.0.1/a", "http://100.64.0.1/a", "http://example.com:99999/a", "http://example.com:/a", "https://example.com/a b", "https://example.com/a<b"} {
		if ValidateURL(raw) == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	for raw, want := range map[string]string{"HTTPS://CDN.Example:8443/Live/{A}|b.php?Id=X#.m3u8": "https://cdn.example:8443/Live/{A}|b.php?Id=X#.m3u8", "http://[2001:DB8::1]/a": "http://[2001:db8::1]/a"} {
		if got := Normalize(raw); got != want {
			t.Errorf("Normalize(%s) = %s", raw, got)
		}
	}
	for _, ip := range []string{"127.0.0.1", "::1", "169.254.169.254", "10.0.0.1", "100.64.0.1", "192.168.1.1", "::ffff:127.0.0.1", "fc00::1", "198.18.0.1"} {
		if PublicAddress(net.ParseIP(ip)) {
			t.Errorf("accepted IP %s", ip)
		}
	}
	c, err := NewClient("")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()
	if _, err := c.Get("http://127.0.0.1:9/metadata"); err == nil {
		t.Fatal("private target fetched")
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestRedirectCannotReachPrivateAddress(t *testing.T) {
	c, _ := NewClient("")
	calls := 0
	c.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"http://169.254.169.254/latest"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})
	if _, err := c.Get("https://public.example/list"); err == nil || calls != 1 {
		t.Fatalf("redirect guard: %v calls=%d", err, calls)
	}
}
