// Package source handles ordinary HTTP media and M3U subscriptions without yt-dlp.
package source

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
)

func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" || len(raw) > 8192 {
		return errors.New("来源必须为有效的 HTTP/HTTPS 地址，不能包含用户名密码或片段")
	}
	if strings.ContainsAny(raw, "\"<>\\") || strings.IndexFunc(raw, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 || strings.HasSuffix(u.Host, ":") {
		return errors.New("来源地址包含不支持的字符")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("来源端口无效")
		}
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, "%") || host == "localhost" || strings.HasSuffix(host, ".localhost") || (net.ParseIP(host) != nil && !PublicAddress(net.ParseIP(host))) {
		return errors.New("来源必须使用公网地址")
	}
	return nil
}

func PublicAddress(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	// Exclude shared address space and special IPv4 ranges used by cloud networks.
	for _, cidr := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4"} {
		_, block, _ := net.ParseCIDR(cidr)
		if block.Contains(ip) {
			return false
		}
	}
	return true
}

type guardedTransport struct {
	base  http.RoundTripper
	proxy bool
}

func (t guardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := ValidateURL(req.URL.String()); err != nil {
		return nil, err
	}
	if t.proxy {
		// The configured proxy is a trusted egress, but do not forward known
		// private destinations to it. Direct connections also check the dial IP.
		addresses, err := net.DefaultResolver.LookupIPAddr(req.Context(), req.URL.Hostname())
		if err != nil {
			return nil, errors.New("无法解析来源域名")
		}
		for _, addr := range addresses {
			if !PublicAddress(addr.IP) {
				return nil, errors.New("来源解析到了非公网地址")
			}
		}
		if len(addresses) == 0 {
			return nil, errors.New("来源域名没有可用地址")
		}
	}
	return t.base.RoundTrip(req)
}
func (t guardedTransport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

func NewClient(proxy string) (*http.Client, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	t := &http.Transport{ForceAttemptHTTP2: true, MaxIdleConns: 16, MaxIdleConnsPerHost: 8, IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second, MaxResponseHeaderBytes: 64 << 10, DisableCompression: true}
	if proxy != "" && proxy != "direct" && proxy != "inherit" {
		p, err := url.Parse(proxy)
		if err != nil || p.Hostname() == "" || (p.Scheme != "http" && p.Scheme != "https" && p.Scheme != "socks5") {
			return nil, errors.New("代理地址无效")
		}
		t.Proxy = http.ProxyURL(p)
	} else {
		dialer.ControlContext = func(_ context.Context, _, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if !PublicAddress(net.ParseIP(host)) {
				return errors.New("来源解析到了非公网地址")
			}
			return nil
		}
	}
	t.DialContext = dialer.DialContext
	return &http.Client{Transport: guardedTransport{base: t, proxy: t.Proxy != nil}, Timeout: 35 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("来源重定向过多")
		}
		return ValidateURL(req.URL.String())
	}}, nil
}
