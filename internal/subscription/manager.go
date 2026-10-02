package subscription

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
	ApplySubscription(context.Context, core.Subscription, source.Playlist) ([]string, error)
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
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	list, err := m.fetch(ctx, sub)
	if err == nil {
		var ids []string
		ids, err = m.repo.ApplySubscription(ctx, sub, list)
		if err == nil {
			for _, id := range ids {
				m.invalidate(id)
			}
			slog.Info("M3U 订阅同步成功", "subscription", sub.ID, "channels", len(list.Entries), "changed", len(ids), "skipped", list.Skipped)
			return nil
		}
		if errors.Is(err, store.ErrValidation) {
			err = errors.New("订阅未保存：" + strings.TrimPrefix(err.Error(), store.ErrValidation.Error()+": "))
		} else {
			err = errors.New("订阅保存失败或配置已变化，现有频道已保留")
		}
	}
	// Persist a bounded diagnostic even when the HTTP caller disconnects.
	recordCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	_ = m.repo.SubscriptionAttempt(recordCtx, sub, err.Error())
	slog.Warn("M3U 订阅同步失败", "subscription", sub.ID, "error", err.Error())
	return err
}
func (m *Manager) fetch(ctx context.Context, sub core.Subscription) (source.Playlist, error) {
	settings, err := m.repo.Settings(ctx)
	if err != nil {
		return source.Playlist{}, errors.New("无法读取代理设置")
	}
	proxy := core.EffectiveProxy(core.Channel{Proxy: sub.Proxy}, settings)
	client, err := m.client(proxy)
	if err != nil {
		return source.Playlist{}, errors.New("无法创建订阅连接，请检查代理设置")
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sub.URL, nil)
	if err != nil {
		return source.Playlist{}, errors.New("订阅地址无效")
	}
	req.Header.Set("User-Agent", "IPTV-Manager/0.2")
	resp, err := client.Do(req)
	if err != nil {
		return source.Playlist{}, errors.New("订阅请求失败，请检查地址、服务器出口或代理")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return source.Playlist{}, fmt.Errorf("订阅来源返回 HTTP %d，现有频道已保留", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, source.MaxPlaylistBytes+1))
	if err != nil {
		return source.Playlist{}, errors.New("订阅内容读取失败，现有频道已保留")
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
