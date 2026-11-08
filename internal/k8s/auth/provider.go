// Package auth provides the abstraction that builds connection info (rest.Config) for managed
// clusters. Provider implementations (eks, kubeconfig, ...) live in subpackages and the
// Multiplexer picks one by Cluster.Provider.Type.
//
// Callers should use the import alias `k8sauth`.
package auth

import (
	"context"
	"fmt"
	"log/slog"

	"k8s.io/client-go/rest"

	"github.com/murasame29/workload-knocker/internal/config"
)

// EndpointProvider builds K8s API connection info from a Cluster.
// Bearer token rotation is expected to happen via WrapTransport.
type EndpointProvider interface {
	// Type is the identifier matching config.ClusterProvider.Type (e.g. "eks").
	Type() string
	// RestConfig returns the rest.Config for the cluster.
	RestConfig(ctx context.Context, c config.Cluster) (*rest.Config, error)
}

// Multiplexer dispatches registered EndpointProviders by Cluster.Provider.Type.
type Multiplexer struct {
	providers map[string]EndpointProvider
}

// NewMultiplexer aggregates EndpointProviders.
// If the same Type appears more than once, the last wins and a warning is logged.
func NewMultiplexer(providers ...EndpointProvider) *Multiplexer {
	m := &Multiplexer{providers: make(map[string]EndpointProvider, len(providers))}
	for _, p := range providers {
		if p == nil {
			continue
		}
		if _, ok := m.providers[p.Type()]; ok {
			slog.Warn("k8s/auth: duplicate provider type registered, last one wins", slog.String("provider.type", p.Type()))
		}
		m.providers[p.Type()] = p
	}
	return m
}

// RestConfig dispatches to the provider matching Cluster.Provider.Type.
// An unregistered type is an error.
func (m *Multiplexer) RestConfig(ctx context.Context, c config.Cluster) (*rest.Config, error) {
	p, ok := m.providers[c.Provider.Type]
	if !ok {
		return nil, fmt.Errorf("k8s/auth: no provider registered for type %q (cluster env=%q)", c.Provider.Type, c.Env)
	}
	return p.RestConfig(ctx, c)
}
