// Package kubeconfig is an EndpointProvider that builds K8s API connection info from a kubeconfig file,
// for local development and arbitrary clusters (KIND, k3d, etc.).
package kubeconfig

import (
	"context"
	"fmt"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/murasame29/workload-knocker/internal/config"
)

// Provider is an auth.EndpointProvider backed by a kubeconfig file.
type Provider struct{}

// New returns a Provider.
func New() *Provider { return &Provider{} }

// Type implements the auth.EndpointProvider contract.
func (p *Provider) Type() string { return config.ProviderTypeKubeconfig }

// RestConfig builds a rest.Config for the cluster from the kubeconfig file.
// An empty Cluster.Provider.Kubeconfig.Context uses the current-context.
func (p *Provider) RestConfig(_ context.Context, c config.Cluster) (*rest.Config, error) {
	if c.Provider.Kubeconfig == nil {
		return nil, fmt.Errorf("kubeconfig provider: Provider.Kubeconfig is nil (cluster env=%q)", c.Env)
	}
	loadingRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: c.Provider.Kubeconfig.Path}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: c.Provider.Kubeconfig.Context}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", c.Provider.Kubeconfig.Path, err)
	}
	return cfg, nil
}
