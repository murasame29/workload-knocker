// Package eks は AWS EKS への接続情報を生成する EndpointProvider 実装。
//
// 動作:
//   - eks:DescribeCluster で endpoint と CA を取得し 5 分キャッシュ
//   - 各 K8s リクエスト直前に aws-iam-authenticator で bearer token を生成
//   - Cluster.Provider.EKS.AssumeRoleArn が設定されている場合は cross-account AssumeRole
package eks

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"golang.org/x/sync/singleflight"
	"k8s.io/client-go/rest"
	tokenpkg "sigs.k8s.io/aws-iam-authenticator/pkg/token"

	"github.com/murasame29/workload-knocker/internal/config"
)

// MetricsHook は Provider がメトリクスを記録するための最小インターフェース。
// metrics.MetricsManager と互換 (subset)。
type MetricsHook interface {
	IncTokenRefresh(cluster, result string)
}

// tokenGenerator は aws-iam-authenticator の Generator のうち、bearer token を生成するメソッドだけを抽象化したもの。
// テストで fake を差し込めるように private interface として持つ。
type tokenGenerator interface {
	GetWithOptions(ctx context.Context, options *tokenpkg.GetTokenOptions) (tokenpkg.Token, error)
}

// describeCacheEntry は eks:DescribeCluster の TTL キャッシュエントリ。
type describeCacheEntry struct {
	endpoint string
	caData   []byte
	expires  time.Time
}

// Provider は AWS EKS 用の auth.EndpointProvider 実装。
type Provider struct {
	awsCfg  aws.Config
	metrics MetricsHook
	gen     tokenGenerator

	mu         sync.Mutex
	descCache  map[string]describeCacheEntry // key = config.Cluster.Env
	descTTL    time.Duration
	descFlight singleflight.Group // describe の thundering-herd 防止 (key = c.Env)
}

// New は IRSA を前提とした EKS Provider を生成する。
// awsCfg は呼び出し側で `awsconfig.LoadDefaultConfig` 等から構築する想定。
// metrics に nil を渡しても安全 (各メトリクス記録は no-op になる)。
func New(awsCfg aws.Config, metrics MetricsHook) (*Provider, error) {
	gen, err := tokenpkg.NewGenerator(true, false)
	if err != nil {
		return nil, fmt.Errorf("aws-iam-authenticator NewGenerator: %w", err)
	}
	return &Provider{
		awsCfg:    awsCfg,
		metrics:   metrics,
		gen:       gen,
		descCache: make(map[string]describeCacheEntry),
		descTTL:   5 * time.Minute,
	}, nil
}

// Type は auth.EndpointProvider 契約。
func (p *Provider) Type() string { return config.ProviderTypeEKS }

// RestConfig は cluster 用の rest.Config を生成する。
// bearer token は WrapTransport 経由でリクエスト直前に都度生成される。
func (p *Provider) RestConfig(ctx context.Context, c config.Cluster) (*rest.Config, error) {
	endpoint, caData, err := p.describe(ctx, c)
	if err != nil {
		return nil, err
	}

	cfg := &rest.Config{
		Host: endpoint,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: caData,
		},
	}
	cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		return &tokenRoundTripper{base: rt, provider: p, cluster: c}
	}
	cfg.Timeout = 30 * time.Second
	return cfg, nil
}

// awsConfigForCluster は Cluster の設定に基づいて aws.Config を返す。
// AssumeRoleArn 指定時は stscreds で一時 credentials に切り替える。
func (p *Provider) awsConfigForCluster(c config.Cluster) aws.Config {
	cfg := p.awsCfg.Copy()
	cfg.Region = c.Provider.EKS.Region

	if c.Provider.EKS.AssumeRoleArn == "" {
		return cfg
	}

	stsClient := sts.NewFromConfig(p.awsCfg)
	provider := stscreds.NewAssumeRoleProvider(stsClient, c.Provider.EKS.AssumeRoleArn, func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = "workload-knocker-" + c.Env
	})
	cfg.Credentials = aws.NewCredentialsCache(provider)
	return cfg
}

// describe は eks:DescribeCluster の結果を 5 分キャッシュする。
// キャッシュミス時は singleflight.Group で同一 cluster への並列 API 呼び出しを 1 回に集約する。
func (p *Provider) describe(ctx context.Context, c config.Cluster) (string, []byte, error) {
	p.mu.Lock()
	if entry, ok := p.descCache[c.Env]; ok && time.Now().Before(entry.expires) {
		p.mu.Unlock()
		return entry.endpoint, entry.caData, nil
	}
	p.mu.Unlock()

	type descResult struct {
		endpoint string
		caData   []byte
	}
	v, err, _ := p.descFlight.Do(c.Env, func() (any, error) {
		// flight 内で再度キャッシュを確認 (先行 flight が書き込んでいる可能性)
		p.mu.Lock()
		if entry, ok := p.descCache[c.Env]; ok && time.Now().Before(entry.expires) {
			p.mu.Unlock()
			return descResult{endpoint: entry.endpoint, caData: entry.caData}, nil
		}
		p.mu.Unlock()

		regionalCfg := p.awsConfigForCluster(c)
		client := eks.NewFromConfig(regionalCfg)

		out, err := client.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: &c.Name})
		if err != nil {
			return nil, fmt.Errorf("eks:DescribeCluster %s: %w", c.Name, err)
		}
		if out.Cluster == nil || out.Cluster.Endpoint == nil ||
			out.Cluster.CertificateAuthority == nil || out.Cluster.CertificateAuthority.Data == nil {
			return nil, fmt.Errorf("eks:DescribeCluster %s: missing endpoint or CA", c.Name)
		}

		endpoint := *out.Cluster.Endpoint
		caB64 := *out.Cluster.CertificateAuthority.Data
		caData, err := base64.StdEncoding.DecodeString(caB64)
		if err != nil {
			return nil, fmt.Errorf("decode CA for %s: %w", c.Name, err)
		}

		p.mu.Lock()
		p.descCache[c.Env] = describeCacheEntry{
			endpoint: endpoint,
			caData:   caData,
			expires:  time.Now().Add(p.descTTL),
		}
		p.mu.Unlock()

		return descResult{endpoint: endpoint, caData: caData}, nil
	})
	if err != nil {
		return "", nil, err
	}
	res := v.(descResult)
	return res.endpoint, res.caData, nil
}

// generateToken は aws-iam-authenticator で bearer token を生成する。
// AssumeRoleArn 指定時は authenticator 側で AssumeRole する。
func (p *Provider) generateToken(ctx context.Context, c config.Cluster) (string, error) {
	tokenOpts := &tokenpkg.GetTokenOptions{
		ClusterID:     c.Name,
		Region:        c.Provider.EKS.Region,
		AssumeRoleARN: c.Provider.EKS.AssumeRoleArn,
	}
	tok, err := p.gen.GetWithOptions(ctx, tokenOpts)
	if err != nil {
		if p.metrics != nil {
			p.metrics.IncTokenRefresh(c.Name, "error")
		}
		return "", fmt.Errorf("generate token for %s: %w", c.Name, err)
	}
	if p.metrics != nil {
		p.metrics.IncTokenRefresh(c.Name, "success")
	}
	return tok.Token, nil
}

// tokenRoundTripper は各リクエスト直前に bearer token を生成する transport ラッパ。
type tokenRoundTripper struct {
	base     http.RoundTripper
	provider *Provider
	cluster  config.Cluster
}

func (t *tokenRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := t.provider.generateToken(req.Context(), t.cluster)
	if err != nil {
		return nil, err
	}
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+tok)
	return t.base.RoundTrip(cloned)
}

// 型アサーション
var _ http.RoundTripper = (*tokenRoundTripper)(nil)
var _ tokenGenerator = (tokenpkg.Generator)(nil)
