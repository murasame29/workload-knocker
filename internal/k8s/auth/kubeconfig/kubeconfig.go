// Package kubeconfig は kubeconfig ファイル経由で K8s API への接続情報を生成する
// EndpointProvider 実装。ローカル開発や任意の cluster (KIND, k3d 等) を扱うために使う。
package kubeconfig

import (
	"context"
	"fmt"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/murasame29/workload-knocker/internal/config"
)

// Provider は kubeconfig ファイル経由の auth.EndpointProvider 実装。
type Provider struct{}

// New は Provider を返す。引数なしのシンプル実装。
func New() *Provider { return &Provider{} }

// Type は auth.EndpointProvider 契約。
func (p *Provider) Type() string { return config.ProviderTypeKubeconfig }

// RestConfig は cluster 用の rest.Config を kubeconfig ファイルから生成する。
// Cluster.Provider.Kubeconfig.Context が空なら current-context を使う。
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
