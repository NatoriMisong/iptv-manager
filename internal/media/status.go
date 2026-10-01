package media

import (
	"time"
	"youtube-tv/internal/core"
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
	s.resolver.Invalidate(id)
	s.mu.Lock()
	delete(s.failures, id)
	s.mu.Unlock()
}

func (s *Server) reportFailure(id, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.failures) >= 512 {
		for key := range s.failures {
			delete(s.failures, key)
			break
		}
	}
	s.failures[id] = core.ChannelStatus{State: "error", Message: message, UpdatedAt: time.Now()}
}

func (s *Server) clearFailure(id string) { s.mu.Lock(); delete(s.failures, id); s.mu.Unlock() }
