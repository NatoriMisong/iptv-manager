package media

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

func ValidateUpstream(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return errors.New("不允许的上游地址")
	}
	host := strings.ToLower(u.Hostname())
	for _, domain := range []string{"youtube.com", "googlevideo.com"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return nil
		}
	}
	return errors.New("上游资源必须属于 YouTube 媒体域名")
}

func publicAddress(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified()
}

func newClient(proxy string) (*http.Client, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		ForceAttemptHTTP2: true, MaxIdleConns: 16, MaxIdleConnsPerHost: 8,
		IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second, MaxResponseHeaderBytes: 64 * 1024,
		DisableCompression: true,
	}
	// Never implicitly inherit HTTP_PROXY: the extractor and media fetcher must
	// use the same explicitly selected route.
	if proxy != "" {
		p, err := url.Parse(proxy)
		if err != nil {
			return nil, errors.New("代理地址无效")
		}
		transport.Proxy = http.ProxyURL(p)
		transport.DialContext = dialer.DialContext
	} else {
		// Check the actual resolved address before connect while retaining Go's
		// IPv4/IPv6 fallback. Serially dialing DNS answers can consume the whole
		// segment timeout when a CDN's first address family is unreachable.
		dialer.ControlContext = func(_ context.Context, _, addr string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return err
			}
			if !publicAddress(net.ParseIP(host)) {
				return errors.New("上游解析到了非公网地址")
			}
			return nil
		}
		transport.DialContext = dialer.DialContext
	}
	return &http.Client{
		Transport: transport, Timeout: 35 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("上游重定向过多")
			}
			return ValidateUpstream(req.URL.String())
		},
	}, nil
}
