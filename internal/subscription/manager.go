package subscription

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"iptv-manager/internal/core"
	"iptv-manager/internal/source"
	"iptv-manager/internal/store"
)

var ErrBusy = errors.New("已有订阅正在同步，请稍后重试")

type Repository interface {
	Subscriptions(context.Context) ([]core.Subscription, error)
	Subscription(context.Context, string) (core.Subscription, error)
	Settings(context.Context) (core.Settings, error)
	SubscriptionAttempt(context.Context, core.Subscription, string) error
	SubscriptionResult(context.Context, core.Subscription, string) error
	SourceFailed(context.Context, core.Subscription, string, string) error
	ApplySource(context.Context, core.Subscription, string, source.Playlist) ([]string, error)
}
type Manager struct {
	repo       Repository
	invalidate func(string)
	gate       chan struct{}
	client     func(string) (*http.Client, error)
}

func New(repo Repository, invalidate func(string)) *Manager {
	return &Manager{repo: repo, invalidate: invalidate, gate: make(chan struct{}, 1), client: source.NewClient}
}

func (m *Manager) Sync(ctx context.Context, id string) error {
	return m.sync(ctx, id, false)
}

// sync refreshes every list of the subscription. Lists are independent: one
// failing list keeps its previous catalogue and does not block the others.
func (m *Manager) sync(ctx context.Context, id string, automatic bool) error {
	select {
	case m.gate <- struct{}{}:
		defer func() { <-m.gate }()
	default:
		return ErrBusy
	}
	sub, err := m.repo.Subscription(ctx, id)
	if err != nil {
		return errors.New("订阅不存在或暂时无法读取")
	}
	if automatic && (!sub.Enabled || time.Since(sub.LastAttempt) < time.Duration(sub.IntervalMinutes)*time.Minute) {
		return nil
	}
	if err = m.repo.SubscriptionAttempt(ctx, sub, ""); err != nil {
		return errors.New("无法开始同步，订阅可能已修改或删除")
	}
	settings, err := m.repo.Settings(ctx)
	if err != nil {
		return m.finish(sub, errors.New("无法读取代理设置"))
	}
	var failures []string
	for _, src := range sub.Sources {
		if ctx.Err() != nil {
			return m.finish(sub, errors.New("同步已取消"))
		}
		err := m.syncSource(ctx, sub, src, settings)
		if err == nil {
			continue
		}
		if errors.Is(err, store.ErrSubscriptionChanged) || errors.Is(err, store.ErrNotFound) {
			return errors.New("订阅配置已变化，本次结果已丢弃，请重新同步")
		}
		failures = append(failures, sourceLabel(src)+"："+err.Error())
		recordCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		_ = m.repo.SourceFailed(recordCtx, sub, src.ID, err.Error())
		stop()
	}
	if len(failures) > 0 {
		return m.finish(sub, errors.New(strings.Join(failures, "；")))
	}
	return m.finish(sub, nil)
}

func (m *Manager) finish(sub core.Subscription, err error) error {
	// Persist a bounded diagnostic even when the HTTP caller disconnects.
	recordCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	message := ""
	if err != nil {
		message = err.Error()
		slog.Warn("自定义订阅同步失败", "subscription", sub.ID, "error", message)
	} else {
		slog.Info("自定义订阅同步成功", "subscription", sub.ID, "sources", len(sub.Sources))
	}
	_ = m.repo.SubscriptionResult(recordCtx, sub, message)
	return err
}

func sourceLabel(src core.SubscriptionSource) string {
	if u, err := url.Parse(src.URL); err == nil && u.Host != "" {
		return u.Host
	}
	return "列表"
}

func (m *Manager) syncSource(ctx context.Context, sub core.Subscription, src core.SubscriptionSource, settings core.Settings) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	list, err := m.fetch(ctx, sub, src, settings)
	if err != nil {
		return err
	}
	ids, err := m.repo.ApplySource(ctx, sub, src.ID, list)
	if err != nil {
		if errors.Is(err, store.ErrValidation) {
			return errors.New("列表未保存：" + strings.TrimPrefix(err.Error(), store.ErrValidation.Error()+": "))
		}
		if errors.Is(err, store.ErrSubscriptionChanged) || errors.Is(err, store.ErrNotFound) {
			return err
		}
		return errors.New("列表保存失败，现有频道已保留")
	}
	for _, id := range ids {
		m.invalidate(id)
	}
	slog.Info("订阅列表同步成功", "subscription", sub.ID, "source", src.ID, "entries", len(list.Entries), "changed", len(ids), "skipped", list.Skipped)
	return nil
}
func (m *Manager) fetch(ctx context.Context, sub core.Subscription, src core.SubscriptionSource, settings core.Settings) (source.Playlist, error) {
	proxy := core.EffectiveProxy(core.Channel{Proxy: sub.Proxy}, settings)
	client, err := m.client(proxy)
	if err != nil {
		return source.Playlist{}, errors.New("无法创建订阅连接，请检查代理设置")
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return source.Playlist{}, errors.New("列表地址无效")
	}
	req.Header.Set("User-Agent", "IPTV-Manager/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return source.Playlist{}, errors.New("列表请求失败，请检查地址、服务器出口或代理")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return source.Playlist{}, fmt.Errorf("列表来源返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, source.MaxPlaylistBytes+1))
	if err != nil {
		return source.Playlist{}, errors.New("列表内容读取失败")
	}
	return source.ParseM3U(body, resp.Request.URL)
}
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		m.syncDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (m *Manager) syncDue(ctx context.Context) {
	subs, err := m.repo.Subscriptions(ctx)
	if err != nil {
		return
	}
	for _, sub := range subs {
		if ctx.Err() != nil {
			return
		}
		if sub.Enabled && time.Since(sub.LastAttempt) >= time.Duration(sub.IntervalMinutes)*time.Minute {
			_ = m.sync(ctx, sub.ID, true)
		}
	}
}
