package web

import (
	"iptv-manager/internal/provider"
	"net/http"
)

func (s *server) builtinSources(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusOK, map[string]any{"sources": provider.Catalogs()})
}
