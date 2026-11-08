// Package auth は managed cluster の K8s API への接続情報 (rest.Config) を生成する
// 抽象を提供する。プロバイダーごとの実装 (eks, kubeconfig, ...) は配下のサブパッケージに置き、
// Multiplexer が Cluster.Provider.Type で振り分ける。
//
// 利用側では import alias `k8sauth` を推奨 (例: import k8sauth ".../internal/k8s/auth")。
package auth

import (
	"context"
	"fmt"
	"log/slog"

	"k8s.io/client-go/rest"

	"github.com/murasame29/workload-knocker/internal/config"
)

// EndpointProvider は Cluster から K8s API への接続情報を生成する。
// bearer token の rotation 等は WrapTransport で行う想定。
type EndpointProvider interface {
	// Type は config.ClusterProvider.Type と対応する識別子 (例: "eks")。
	Type() string
	// RestConfig は cluster 用の rest.Config を返す。
	RestConfig(ctx context.Context, c config.Cluster) (*rest.Config, error)
}

// Multiplexer は登録された EndpointProvider を Cluster.Provider.Type で dispatch する。
type Multiplexer struct {
	providers map[string]EndpointProvider
}

// NewMultiplexer は EndpointProvider 群を集約する。
// 同一 Type が複数渡された場合は後勝ちで上書きし、警告ログを出す。
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

// RestConfig は Cluster.Provider.Type に対応する provider に dispatch する。
// 未登録の type は error。
func (m *Multiplexer) RestConfig(ctx context.Context, c config.Cluster) (*rest.Config, error) {
	p, ok := m.providers[c.Provider.Type]
	if !ok {
		return nil, fmt.Errorf("k8s/auth: no provider registered for type %q (cluster env=%q)", c.Provider.Type, c.Env)
	}
	return p.RestConfig(ctx, c)
}
