// Package httpserver implements the HTTP server exposing /metrics and /api/v1/stop-all
// as a server.Lifecycle.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
)

const componentName = "http"

// Server wraps net/http.Server to satisfy server.Lifecycle.
//
// Probe endpoints:
//   - /livez, /healthz: always 200 (process liveness)
//   - /readyz: 200 only while ready is true, 503 before that
type Server struct {
	server *http.Server
	ready  atomic.Bool
}

// New returns a Server with metrics and stop-all / start registered on the mux.
func New(port int, metricsHandler http.Handler, stopAllHandler http.Handler, startHandler http.Handler) *Server {
	s := &Server{}
	mux := http.NewServeMux()
	mux.Handle("/metrics", metricsHandler)
	mux.HandleFunc("/livez", s.handleLive)
	mux.HandleFunc("/healthz", s.handleLive)
	mux.HandleFunc("/readyz", s.handleReady)
	mux.Handle("/api/v1/stop-all", stopAllHandler)
	mux.Handle("/api/v1/start", startHandler)
	s.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}
	return s
}

func (s *Server) Name() string { return componentName }

// MarkReady switches /readyz to 200 after Bootstrap completes.
func (s *Server) MarkReady() { s.ready.Store(true) }

// MarkNotReady returns /readyz to 503 when graceful shutdown starts.
func (s *Server) MarkNotReady() { s.ready.Store(false) }

// Start runs the HTTP server, blocking. Stop it via Shutdown.
func (s *Server) Start(_ context.Context) error {
	if err := s.server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown stops gracefully; http.ErrServerClosed on a repeated stop is ignored (idempotent).
func (s *Server) Shutdown(ctx context.Context) error {
	s.MarkNotReady()
	err := s.server.Shutdown(ctx)
	if err == nil {
		return nil
	}
	if errors.Is(err, http.ErrServerClosed) {
		slog.Info("http server is already closed")
		return nil
	}
	return err
}

func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready"))
}
