// Package reload hot reloads the cluster registry and the authorization policy from files.
package reload

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// DefaultDebounce coalesces the burst of events one file update produces.
const DefaultDebounce = 500 * time.Millisecond

// kubeDataLink is the symlink Kubernetes atomically repoints when a ConfigMap volume updates.
const kubeDataLink = "..data"

// Watchers is the set of reload watchers handed to the lifecycle manager.
type Watchers []*Watcher

// Watcher is a server.Lifecycle that calls onChange (debounced, serialized) when path changes.
// It watches the parent directory so atomic renames and ConfigMap symlink swaps are seen.
type Watcher struct {
	name     string
	path     string
	debounce time.Duration
	onChange func(ctx context.Context)
	done     chan struct{}
}

// NewWatcher builds a Watcher; name labels logs and the lifecycle component.
func NewWatcher(name, path string, debounce time.Duration, onChange func(ctx context.Context)) *Watcher {
	return &Watcher{name: name, path: path, debounce: debounce, onChange: onChange, done: make(chan struct{})}
}

// Name implements server.Lifecycle.
func (w *Watcher) Name() string { return "reload-" + w.name }

// Start blocks until ctx is canceled. A watch setup failure is logged and hot reload stays off;
// it never stops the controller.
func (w *Watcher) Start(ctx context.Context) error {
	defer close(w.done)

	fw, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Warn("hot reload disabled: cannot create file watcher", slog.String("target", w.name), slog.String("error.message", err.Error()))
		<-ctx.Done()
		return nil
	}
	defer func() { _ = fw.Close() }()

	dir := filepath.Dir(w.path)
	if err := fw.Add(dir); err != nil {
		slog.Warn("hot reload disabled: cannot watch directory", slog.String("target", w.name), slog.String("dir", dir), slog.String("error.message", err.Error()))
		<-ctx.Done()
		return nil
	}
	slog.Info("watching for config changes", slog.String("target", w.name), slog.String("path", w.path))

	timer := time.NewTimer(w.debounce)
	timer.Stop()
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-fw.Events:
			if !ok {
				return nil
			}
			if w.relevant(ev) {
				timer.Reset(w.debounce)
			}
		case err, ok := <-fw.Errors:
			if !ok {
				return nil
			}
			slog.Warn("file watcher error", slog.String("target", w.name), slog.String("error.message", err.Error()))
		case <-timer.C:
			w.onChange(ctx)
		}
	}
}

// Shutdown waits for Start to return (or ctx to expire).
func (w *Watcher) Shutdown(ctx context.Context) error {
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Watcher) relevant(ev fsnotify.Event) bool {
	if ev.Op == fsnotify.Chmod {
		return false
	}
	base := filepath.Base(ev.Name)
	return base == filepath.Base(w.path) || base == kubeDataLink
}
