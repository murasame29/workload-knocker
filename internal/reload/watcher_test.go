package reload

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

const testDebounce = 200 * time.Millisecond

// startWatcher runs a Watcher on path and returns a counter of onChange calls.
func startWatcher(t *testing.T, path string) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	w := NewWatcher("test", path, testDebounce, func(context.Context) { calls.Add(1) })
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	go func() {
		close(started)
		_ = w.Start(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer scancel()
		if err := w.Shutdown(sctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	<-started
	// fsnotify registers the directory inside Start; give it a moment before the first write.
	time.Sleep(100 * time.Millisecond)
	return &calls
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWatcher_DebouncesBurstOfWrites(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("v0"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := startWatcher(t, path)

	for i := range 5 {
		if err := os.WriteFile(path, []byte{byte('a' + i)}, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "a reload", func() bool { return calls.Load() >= 1 })
	time.Sleep(4 * testDebounce)
	if got := calls.Load(); got != 1 {
		t.Errorf("onChange calls = %d, want 1 for one burst", got)
	}
}

func TestWatcher_IgnoresOtherFilesInTheDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	calls := startWatcher(t, path)

	if err := os.WriteFile(filepath.Join(dir, "other.yaml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(6 * testDebounce)
	if got := calls.Load(); got != 0 {
		t.Errorf("onChange calls = %d, want 0", got)
	}
}

func TestWatcher_SeesFileCreatedAfterStart(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "authz.yaml")
	calls := startWatcher(t, path)

	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a reload", func() bool { return calls.Load() >= 1 })
}

func TestWatcher_SeesAtomicRename(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := startWatcher(t, path)

	tmp := filepath.Join(dir, "config.yaml.tmp")
	if err := os.WriteFile(tmp, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a reload", func() bool { return calls.Load() >= 1 })
}

// TestWatcher_SeesConfigMapSymlinkSwap reproduces the kubelet layout:
// config.yaml -> ..data/config.yaml and ..data -> ..<timestamp>, repointed atomically.
func TestWatcher_SeesConfigMapSymlinkSwap(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	v1 := filepath.Join(dir, "..2026_01_01_00_00_00.1")
	if err := os.Mkdir(v1, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v1, "config.yaml"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(v1), filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(filepath.Join("..data", "config.yaml"), path); err != nil {
		t.Fatal(err)
	}
	calls := startWatcher(t, path)

	v2 := filepath.Join(dir, "..2026_01_01_00_01_00.2")
	if err := os.Mkdir(v2, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v2, "config.yaml"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmpLink := filepath.Join(dir, "..data_tmp")
	if err := os.Symlink(filepath.Base(v2), tmpLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpLink, filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(v1); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "a reload after the ..data swap", func() bool { return calls.Load() >= 1 })
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "two" {
		t.Errorf("file through the symlink = %q, %v; want the new content", got, err)
	}
}

func TestWatcher_MissingDirectoryDoesNotStopTheController(t *testing.T) {
	t.Parallel()

	w := NewWatcher("test", filepath.Join(t.TempDir(), "missing", "authz.yaml"), testDebounce, func(context.Context) {})
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- w.Start(ctx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Start = %v, want nil so the other components keep running", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
}

func TestWatcher_EndToEndRegistryReload(t *testing.T) {
	t.Parallel()

	f := newRegistryFixture(t, registryYAML("slack", "stg"))
	startWatcher2 := NewWatcher("registry", f.path, testDebounce, f.r.Reload)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = startWatcher2.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = startWatcher2.Shutdown(context.Background())
	})
	time.Sleep(100 * time.Millisecond)

	f.write(t, "not: [valid")
	waitFor(t, "the error counter", func() bool { return f.m.count("registry/error") == 1 })
	f.write(t, registryYAML("slack", "stg", "dev"))
	waitFor(t, "the new cluster", func() bool { _, ok := f.r.Config.BySlashCommand("/wk-dev"); return ok })
	f.write(t, registryYAML("slack", "dev"))
	waitFor(t, "stg removal", func() bool { _, ok := f.r.Config.BySlashCommand("/wk-stg"); return !ok })
	if f.ms.Namespaces("stg") != nil {
		t.Error("stg store must be gone after removal")
	}
}
