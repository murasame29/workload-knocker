// Package server provides the Lifecycle abstraction for starting and gracefully shutting down
// long-running components (HTTP, Slack, etc.).
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

// Lifecycle abstracts starting and gracefully shutting down a background component.
// Start returns on ctx cancellation; Shutdown is idempotent.
type Lifecycle interface {
	Name() string
	Start(ctx context.Context) error
	Shutdown(ctx context.Context) error
}

// Manager is a supervisor that starts Lifecycles in parallel via errgroup and
// sends Shutdown to all of them on ctx.Done().
type Manager struct {
	components      []Lifecycle
	shutdownTimeout time.Duration
}

// NewManager builds a Manager aggregating Lifecycles.
// shutdownTimeout applies independently to each Lifecycle.Shutdown.
func NewManager(shutdownTimeout time.Duration, components ...Lifecycle) *Manager {
	return &Manager{
		components:      components,
		shutdownTimeout: shutdownTimeout,
	}
}

// Run starts all Lifecycles in parallel, drives Shutdown on ctx completion and returns the errgroup.Wait result.
// It waits for shutdownAll to finish, so in-flight handlers complete before the caller exits the process.
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
