package rollout

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"k8s.io/client-go/rest"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/k8s"
)

// RestConfigSource obtains a *rest.Config for a cluster (k8sauth.Multiplexer in production).
type RestConfigSource interface {
	RestConfig(ctx context.Context, c config.Cluster) (*rest.Config, error)
}

// ClusterLookup resolves the live cluster of an env (config.Holder in production).
type ClusterLookup interface {
	ByEnv(env string) (config.Cluster, bool)
}

type cachedReader struct {
	cluster config.Cluster
	reader  k8s.RolloutReader
}

// Readers builds one RolloutReader per managed cluster lazily and rebuilds it when the
// cluster definition changes on hot reload.
type Readers struct {
	src      RestConfigSource
	clusters ClusterLookup
	build    func(*rest.Config) (k8s.RolloutReader, error)

	mu    sync.Mutex
	cache map[string]cachedReader
}

var _ ReaderSource = (*Readers)(nil)

// NewReaders returns a ReaderSource backed by real clients.
func NewReaders(src RestConfigSource, clusters ClusterLookup) *Readers {
	return &Readers{src: src, clusters: clusters, build: k8s.NewClusterRolloutReader, cache: map[string]cachedReader{}}
}

// Reader returns the reader of env, or an error when the cluster is gone from the registry.
func (r *Readers) Reader(ctx context.Context, env string) (k8s.RolloutReader, error) {
	cl, ok := r.clusters.ByEnv(env)
	if !ok {
		return nil, fmt.Errorf("cluster %q is not in the registry", env)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.cache[env]; ok && reflect.DeepEqual(c.cluster, cl) {
		return c.reader, nil
	}
	cfg, err := r.src.RestConfig(ctx, cl)
	if err != nil {
		return nil, fmt.Errorf("build rest.Config: %w", err)
	}
	reader, err := r.build(cfg)
	if err != nil {
		return nil, err
	}
	r.cache[env] = cachedReader{cluster: cl, reader: reader}
	return reader, nil
}
