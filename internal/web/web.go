package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dotfrankruan/pi-monitor/internal/monitor"
)

//go:embed static/*
var assets embed.FS

type Server struct {
	monitor *monitor.Monitor
	log     *slog.Logger
	mux     *http.ServeMux
}

func New(mon *monitor.Monitor, logger *slog.Logger) *Server {
	s := &Server{monitor: mon, log: logger, mux: http.NewServeMux()}
	static, _ := fs.Sub(assets, "static")
	s.mux.Handle("GET /", http.FileServer(http.FS(static)))
	s.mux.HandleFunc("GET /api/current", s.current)
	s.mux.HandleFunc("GET /api/history", s.history)
	s.mux.HandleFunc("GET /api/stream", s.stream)
	s.mux.HandleFunc("GET /healthz", s.health)
	return s
}

func (s *Server) Handler() http.Handler {
	return securityHeaders(s.mux)
}

func (s *Server) current(w http.ResponseWriter, _ *http.Request) {
	point, ok := s.monitor.Current()
	if !ok {
		http.Error(w, "no sample available", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, point)
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	to, err := parseTime(r.URL.Query().Get("to"), now)
	if err != nil {
		http.Error(w, "invalid to time", http.StatusBadRequest)
		return
	}
	fromDefault := to.Add(-time.Hour)
	from, err := parseTime(r.URL.Query().Get("from"), fromDefault)
	if err != nil || !from.Before(to) {
		http.Error(w, "invalid from time", http.StatusBadRequest)
		return
	}
	maxPoints := 1200
	if raw := r.URL.Query().Get("max_points"); raw != "" {
		maxPoints, err = strconv.Atoi(raw)
		if err != nil || maxPoints < 2 || maxPoints > 10000 {
			http.Error(w, "max_points must be between 2 and 10000", http.StatusBadRequest)
			return
		}
	}
	points, err := s.monitor.History(r.Context(), from, to, maxPoints)
	if err != nil {
		s.log.Error("history query failed", "error", err)
		http.Error(w, "history query failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"from": from, "to": to, "samples": points})
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	channel, unsubscribe := s.monitor.Subscribe()
	defer unsubscribe()
	if point, ok := s.monitor.Current(); ok {
		writeEvent(w, point)
		flusher.Flush()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case point, open := <-channel:
			if !open {
				return
			}
			writeEvent(w, point)
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, value any) {
	b, _ := json.Marshal(value)
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	point, ok := s.monitor.Current()
	if !ok || time.Since(point.Timestamp) > 10*time.Second {
		http.Error(w, "collector is stale", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ok")
}

func parseTime(value string, fallback time.Time) (time.Time, error) {
	if value == "" {
		return fallback, nil
	}
	if strings.HasPrefix(value, "-") {
		duration, err := time.ParseDuration(strings.TrimPrefix(value, "-"))
		return fallback.Add(-duration), err
	}
	return time.Parse(time.RFC3339, value)
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'")
		next.ServeHTTP(w, r)
	})
}
