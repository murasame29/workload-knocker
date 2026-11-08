// Package httpserver は /metrics と /api/v1/stop-all を公開する HTTP server を
// server.Lifecycle として実装する。
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/newrelic/go-agent/v3/newrelic"
)

const componentName = "http"

// Server は server.Lifecycle を満たす net/http.Server ラッパ。
//
// Probe エンドポイント:
//   - /livez, /healthz: 常に 200 (プロセス生存確認)
//   - /readyz: ready が true の間だけ 200、それ以前は 503
type Server struct {
	server *http.Server
	ready  atomic.Bool
}

// New は metrics と stop-all / start を mux に登録した Server を返す。
// nrApp が non-nil の場合 /api/v1/stop-all と /api/v1/start を New Relic transaction でラップする。
func New(port int, metricsHandler http.Handler, stopAllHandler http.Handler, startHandler http.Handler, nrApp *newrelic.Application) *Server {
	s := &Server{}
	mux := http.NewServeMux()
	mux.Handle("/metrics", metricsHandler)
	mux.HandleFunc("/livez", s.handleLive)
	mux.HandleFunc("/healthz", s.handleLive)
	mux.HandleFunc("/readyz", s.handleReady)
	if nrApp != nil {
		mux.Handle(newrelic.WrapHandle(nrApp, "/api/v1/stop-all", stopAllHandler))
		mux.Handle(newrelic.WrapHandle(nrApp, "/api/v1/start", startHandler))
	} else {
		mux.Handle("/api/v1/stop-all", stopAllHandler)
		mux.Handle("/api/v1/start", startHandler)
	}
	s.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}
	return s
}

func (s *Server) Name() string { return componentName }

// MarkReady は Bootstrap 完了後に呼び /readyz を 200 へ切り替える。
func (s *Server) MarkReady() { s.ready.Store(true) }

// MarkNotReady は graceful shutdown 開始時に呼び /readyz を 503 へ戻す。
func (s *Server) MarkNotReady() { s.ready.Store(false) }

// Start は HTTP サーバーをブロッキングで起動する。停止は Shutdown 経由。
func (s *Server) Start(_ context.Context) error {
	if err := s.server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown は graceful に停止する。冪等性のため二重停止時の http.ErrServerClosed は無害化する。
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
