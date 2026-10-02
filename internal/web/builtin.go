package web

import "net/http"

// The catalog contains public URLs captured from each broadcaster's own site.
// Adding entries uses the normal stream bulk API; no resolver runs on import.
func (s *server) builtinSources(w http.ResponseWriter, r *http.Request) {
	data, err := assets.ReadFile("static/builtin-sources.json")
	if err != nil {
		fail(w, http.StatusInternalServerError, "无法读取内置直播源")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(data)
}
