package store

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/murasame29/workload-knocker/internal/k8s"
	"github.com/murasame29/workload-knocker/internal/notify"
)

type mutableLister struct {
	mu    sync.Mutex
	items []kedav1alpha1.ScaledObject
}

func (l *mutableLister) List(context.Context) ([]kedav1alpha1.ScaledObject, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]kedav1alpha1.ScaledObject(nil), l.items...), nil
}

func (l *mutableLister) set(tmpl string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	so := makeScaledObject("games", "terraria")
	if tmpl != "" {
		so.Annotations = map[string]string{notify.ReadyMessageAnnotation: tmpl}
	}
	l.items = []kedav1alpha1.ScaledObject{*so}
}

type warnCapture struct {
	slog.Handler
	mu       sync.Mutex
	messages []string
}

func (c *warnCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *warnCapture) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn && strings.Contains(r.Message, "ready-message") {
		c.mu.Lock()
		c.messages = append(c.messages, r.Message)
		c.mu.Unlock()
	}
	return nil
}

func (c *warnCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *warnCapture) WithGroup(string) slog.Handler      { return c }

func (c *warnCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.messages)
}

// Not parallel: it swaps the default logger to count the warnings.
func TestClusterStore_ReadyMessageAnnotation(t *testing.T) {
	capture := &warnCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(prev) })

	lister := &mutableLister{}
	lister.set("Connect to {{.Name}}.example.com:7777")
	store := kedaStore(testCluster("stg"), lister, k8s.NewConfigMapStateRepository(fake.NewClientset(), testControllerNs, "stg"), &fakeMetrics{}, nil)
	ctx := context.Background()
	if err := store.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	ready := func() string { return store.WorkloadsByNamespace("games")[0].ReadyMessage }

	if got := ready(); got != "Connect to {{.Name}}.example.com:7777" || capture.count() != 0 {
		t.Fatalf("valid template: ReadyMessage=%q warnings=%d", got, capture.count())
	}

	lister.set("{{.Missing}}")
	for range 3 {
		if err := store.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := ready(); got != "{{.Missing}}" || capture.count() != 1 {
		t.Errorf("invalid template changed once: ReadyMessage=%q warnings=%d, want one warning", got, capture.count())
	}

	lister.set("")
	if err := store.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := ready(); got != "" || capture.count() != 1 {
		t.Errorf("annotation removed: ReadyMessage=%q warnings=%d", got, capture.count())
	}
}
