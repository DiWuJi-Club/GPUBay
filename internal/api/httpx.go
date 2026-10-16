package api

import (
	"net/http"

	"spark-cloud/internal/platform/httpx"
)

func writeJSON(w http.ResponseWriter, status int, v any) { httpx.WriteJSON(w, status, v) }

func ok(w http.ResponseWriter) { httpx.OK(w) }

func list[T any](items []T) map[string]any { return httpx.List(items) }

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	httpx.WriteError(s.Log, w, r, err)
}

func decodeJSON(r *http.Request, v any) error { return httpx.DecodeJSON(r, v) }

func clientIP(r *http.Request) string { return httpx.ClientIP(r) }
