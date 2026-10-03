package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"iptv-manager/internal/core"
	"iptv-manager/internal/source"
	"iptv-manager/internal/store"
)

const proxyTestTimeout = 10 * time.Second
const proxyTestURL = "https://ipip.info"

func (s *server) saveProxy(w http.ResponseWriter, r *http.Request) {
	var p core.Proxy
	if !decode(w, r, &p) {
		return
	}
	p.ID = r.PathValue("id")
	saved, err := s.repo.SaveProxy(r.Context(), p)
	if err != nil {
		if errors.Is(err, store.ErrValidation) {
			fail(w, 400, strings.TrimPrefix(err.Error(), store.ErrValidation.Error()+": "))
		} else if errors.Is(err, store.ErrNotFound) {
			fail(w, 404, "代理不存在")
		} else {
			fail(w, 500, "代理保存失败")
		}
		return
	}
	// An edited address must not be reused by sessions opened through the old one.
	if p.ID != "" {
		s.invalidateAll(r.Context())
	}
	respond(w, 200, saved)
}

func (s *server) deleteProxy(w http.ResponseWriter, r *http.Request) {
	err := s.repo.DeleteProxy(r.Context(), r.PathValue("id"))
	var inUse *store.ProxyInUseError
	switch {
	case err == nil:
		respond(w, 200, map[string]bool{"ok": true})
	case errors.As(err, &inUse):
		respond(w, 409, map[string]any{"error": inUse.Error(), "channels": inUse.Channels, "subscriptions": inUse.Subscriptions, "providers": inUse.Providers})
	case errors.Is(err, store.ErrNotFound):
		fail(w, 404, "代理不存在")
	default:
		fail(w, 500, "代理删除失败")
	}
}

func (s *server) providerProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Proxy string `json:"proxy"`
	}
	if !decode(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	if err := s.repo.SetProviderProxy(r.Context(), id, body.Proxy); err != nil {
		if errors.Is(err, store.ErrValidation) {
			fail(w, 400, strings.TrimPrefix(err.Error(), store.ErrValidation.Error()+": "))
		} else if errors.Is(err, store.ErrNotFound) {
			fail(w, 404, "内置直播源不存在")
		} else {
			fail(w, 500, "代理设置保存失败")
		}
		return
	}
	channels, err := s.repo.Channels(r.Context())
	if err == nil {
		for _, ch := range channels {
			if ch.IsBuiltin() && ch.ProviderID == id {
				s.media.Invalidate(ch.ID)
			}
		}
	}
	respond(w, 200, map[string]bool{"ok": true})
}

// testProxy fetches the egress IP through a saved proxy. It runs only on
// explicit request, one at a time, with no retry.
func (s *server) testProxy(w http.ResponseWriter, r *http.Request) {
	settings, err := s.repo.Settings(r.Context())
	if err != nil {
		fail(w, 500, "无法读取设置")
		return
	}
	p, ok := settings.FindProxy(r.PathValue("id"))
	if !ok {
		fail(w, 404, "代理不存在")
		return
	}
	select {
	case s.proxyTests <- struct{}{}:
		defer func() { <-s.proxyTests }()
	default:
		fail(w, 409, "已有代理测试正在进行，请稍后再试")
		return
	}
	tester := s.opts.ProxyTester
	if tester == nil {
		tester = testProxyEgress
	}
	ctx, cancel := context.WithTimeout(r.Context(), proxyTestTimeout)
	defer cancel()
	started := time.Now()
	ip, err := tester(ctx, p.URL())
	elapsed := time.Since(started).Milliseconds()
	if err != nil {
		respond(w, 502, map[string]any{"error": err.Error(), "elapsed_ms": elapsed})
		return
	}
	respond(w, 200, map[string]any{"ok": true, "ip": ip, "elapsed_ms": elapsed})
}

func testProxyEgress(ctx context.Context, proxyURL string) (string, error) {
	client, err := source.NewClient(proxyURL)
	if err != nil {
		return "", errors.New("代理地址无效")
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxyTestURL, nil)
	if err != nil {
		return "", errors.New("无法创建测试请求")
	}
	req.Header.Set("User-Agent", "curl/8.5.0")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("通过代理访问 ipip.info 失败：%s", proxyTestReason(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ipip.info 返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", errors.New("读取 ipip.info 响应失败")
	}
	ip := net.ParseIP(strings.TrimSpace(string(body)))
	if ip == nil {
		return "", errors.New("ipip.info 返回的内容不是 IP 地址")
	}
	return ip.String(), nil
}

// proxyTestReason classifies transport failures without echoing addresses or
// credentials from the underlying error.
func proxyTestReason(err error) string {
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return "连接超时"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "无法解析域名"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "连接被拒绝"
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "proxyconnect"):
		return "无法连接代理服务器"
	case strings.Contains(text, "socks"):
		return "SOCKS5 代理拒绝连接或认证失败"
	case strings.Contains(text, "407"):
		return "代理要求认证或认证失败"
	case strings.Contains(text, "tls"), strings.Contains(text, "certificate"):
		return "TLS 握手失败"
	case strings.Contains(text, "non-public"), strings.Contains(text, "非公网"):
		return "目标解析到了非公网地址"
	}
	return "网络错误"
}
