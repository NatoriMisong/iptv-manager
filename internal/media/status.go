package media

import (
	"context"
	"time"

	"iptv-manager/internal/core"
)

func (s *Server) Statuses() map[string]core.ChannelStatus {
	statuses := make(map[string]core.ChannelStatus)
	if source, ok := s.resolver.(interface {
		Statuses() map[string]core.ChannelStatus
	}); ok {
		statuses = source.Statuses()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, failure := range s.failures {
		current := statuses[id]
		if time.Since(failure.UpdatedAt) > 10*time.Minute {
			delete(s.failures, id)
			continue
		}
		if current.UpdatedAt.Before(failure.UpdatedAt) {
			statuses[id] = failure
		}
	}
	return statuses
}

func (s *Server) Invalidate(id string) {
	s.providers.Invalidate(id)
	s.resolver.Invalidate(id)
	s.mu.Lock()
	delete(s.failures, id)
	delete(s.selectionRefreshes, id)
	for key, ref := range s.resources {
		if ref.Channel == id && ref.Session != nil {
			delete(s.resources, key)
		}
	}
	s.mu.Unlock()
	s.cache.clearChannel(id)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if ch, err := s.repo.Channel(ctx, id); err == nil {
		if ch.IsStream() {
			s.reportState(id, "unknown", "等待连接原始直播源")
		}
		if ch.IsBuiltin() {
			s.reportState(id, "unknown", "网站来源缓存已清除，等待下次播放")
		}
	}
}

func (s *Server) reportFailure(id, message string) {
	s.reportState(id, "error", message)
}
func (s *Server) reportState(id, state, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.failures) >= 512 {
		for key := range s.failures {
			delete(s.failures, key)
			break
		}
	}
	s.failures[id] = core.ChannelStatus{State: state, Message: message, UpdatedAt: time.Now()}
}

func (s *Server) clearFailure(id string) { s.mu.Lock(); delete(s.failures, id); s.mu.Unlock() }
