package rollout

import (
	"context"
	"testing"

	"k8s.io/client-go/rest"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/k8s"
)

type fakeRestSource struct{ calls int }

func (f *fakeRestSource) RestConfig(context.Context, config.Cluster) (*rest.Config, error) {
	f.calls++
	return &rest.Config{}, nil
}

type fakeLookup map[string]config.Cluster

func (l fakeLookup) ByEnv(env string) (config.Cluster, bool) { c, ok := l[env]; return c, ok }

func TestReaders_CachesPerClusterAndRebuildsOnChange(t *testing.T) {
	t.Parallel()

	src := &fakeRestSource{}
	lookup := fakeLookup{"dev": {Env: "dev", Name: "a"}}
	r := NewReaders(src, lookup)
	r.build = func(*rest.Config) (k8s.RolloutReader, error) { return &fakeReader{}, nil }

	first, err := r.Reader(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := r.Reader(context.Background(), "dev"); again != first || src.calls != 1 {
		t.Errorf("second call must reuse the reader (rest configs built: %d)", src.calls)
	}

	lookup["dev"] = config.Cluster{Env: "dev", Name: "b"}
	if changed, _ := r.Reader(context.Background(), "dev"); changed == first || src.calls != 2 {
		t.Errorf("changed cluster must rebuild the reader (rest configs built: %d)", src.calls)
	}

	if _, err := r.Reader(context.Background(), "gone"); err == nil {
		t.Error("unknown env: want error")
	}
}
