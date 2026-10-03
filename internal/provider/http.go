package provider

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"

	"iptv-manager/internal/source"
)

// Direct connections check the actual dial IP. Explicit proxies resolve only
// broadcaster-approved URLs; redirects must pass the same provider validation.
func newProviderClient(proxy string, validate func(string) error) (*http.Client, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{ForceAttemptHTTP2: true, MaxIdleConns: 16, MaxIdleConnsPerHost: 8, IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second, MaxResponseHeaderBytes: 64 * 1024, DisableCompression: true}
	if proxy != "" {
		p, err := url.Parse(proxy)
		if err != nil {
			return nil, errors.New("代理地址无效")
		}
		transport.Proxy = http.ProxyURL(p)
	} else {
		dialer.ControlContext = func(_ context.Context, _, addr string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return err
			}
			if !source.PublicAddress(net.ParseIP(host)) {
				return errors.New("上游解析到了非公网地址")
			}
			return nil
		}
	}
	transport.DialContext = dialer.DialContext
	client := &http.Client{Transport: transport, Timeout: 35 * time.Second}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("来源重定向过多")
		}
		return validate(req.URL.String())
	}
	return client, nil
}

type providerResponseBody struct {
	io.ReadCloser
	stop   func() bool
	cancel context.CancelFunc
}

func (b *providerResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.stop()
	b.cancel()
	return err
}

func providerNetworkReason(err error) string {
	var remote *net.OpError
	if errors.As(err, &remote) && remote.Op == "remote error" && remote.Err.Error() == "tls: handshake failure" {
		return "tls_handshake_failure"
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "timeout"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns_error"
	}
	var certificate *tls.CertificateVerificationError
	if errors.As(err, &certificate) {
		return "tls_certificate_error"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "connection_closed"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Sprintf("transport_%T", urlErr.Err)
	}
	return "network_error"
}
