// Package server は長時間動作コンポーネント (HTTP / Slack 等) の起動と graceful shutdown を
// 統一的に扱う Lifecycle 抽象を提供する。
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// Lifecycle はバックグラウンドコンポーネントの起動と graceful shutdown を抽象化する。
// Start は ctx キャンセルで戻り、Shutdown は冪等。
type Lifecycle interface {
	Name() string
	Start(ctx context.Context) error
	Shutdown(ctx context.Context) error
}

// Manager は複数 Lifecycle を errgroup で並列起動し、ctx.Done() で全コンポーネントに
// Shutdown を送る supervisor。
type Manager struct {
	components      []Lifecycle
	shutdownTimeout time.Duration
}

// NewManager は Lifecycle を集約した Manager を構築する。
// shutdownTimeout は各 Lifecycle.Shutdown ごとに独立に適用される。
func NewManager(shutdownTimeout time.Duration, components ...Lifecycle) *Manager {
	return &Manager{
		components:      components,
		shutdownTimeout: shutdownTimeout,
	}
}

// Run は全 Lifecycle を並列起動し、ctx 完了を契機に Shutdown を駆動して errgroup.Wait の結果を返す。
// shutdownAll の完了 (各 Lifecycle.Shutdown の wg.Wait) も待ってから戻るため、呼び出し元が
// プロセスを exit させる前に in-flight ハンドラが必ず完了する。
func (m *Manager) Run(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)

	for _, lc := range m.components {
		g.Go(func() error {
			slog.Info("lifecycle starting", slog.String("component", lc.Name()))
			err := lc.Start(gctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("lifecycle Start returned error", slog.String("component", lc.Name()), slog.String("error.message", err.Error()))
				return fmt.Errorf("%s: %w", lc.Name(), err)
			}
			return nil
		})
	}

	shutdownDone := make(chan struct{})
	go func() {
		m.shutdownAll(gctx)
		close(shutdownDone)
	}()

	waitErr := g.Wait()
	<-shutdownDone
	return waitErr
}

func (m *Manager) shutdownAll(ctx context.Context) {
	<-ctx.Done()

	var wg sync.WaitGroup
	for _, lc := range m.components {
		wg.Add(1)
		go func() {
			defer wg.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), m.shutdownTimeout)
			defer cancel()
			slog.Info("lifecycle shutdown initiated", slog.String("component", lc.Name()))
			if err := lc.Shutdown(shutdownCtx); err != nil {
				slog.Warn("lifecycle Shutdown returned error", slog.String("component", lc.Name()), slog.String("error.message", err.Error()))
				return
			}
			slog.Info("lifecycle shutdown completed", slog.String("component", lc.Name()))
		}()
	}
	wg.Wait()
}
