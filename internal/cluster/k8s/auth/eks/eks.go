// Package eks is an EndpointProvider that builds connection info for AWS EKS.
//
// It fetches the endpoint and CA via eks:DescribeCluster (cached 5 min), generates a bearer token
// with aws-iam-authenticator just before each request, and uses cross-account AssumeRole when
// Cluster.Provider.EKS.AssumeRoleArn is set.
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

// MetricsHook is the minimal interface the Provider uses to record metrics (a subset of metrics.MetricsManager).
type MetricsHook interface {
	IncTokenRefresh(cluster, result string)
}

// tokenGenerator abstracts the bearer token method of the aws-iam-authenticator Generator.
// It is private so tests can inject a fake.
type tokenGenerator interface {
	GetWithOptions(ctx context.Context, options *tokenpkg.GetTokenOptions) (tokenpkg.Token, error)
}

// describeCacheEntry is a TTL cache entry for eks:DescribeCluster.
type describeCacheEntry struct {
	endpoint string
	caData   []byte
	expires  time.Time
}

// Provider is the auth.EndpointProvider implementation for AWS EKS.
type Provider struct {
	awsCfg  aws.Config
	metrics MetricsHook
	gen     tokenGenerator

	mu         sync.Mutex
	descCache  map[string]describeCacheEntry // key = config.Cluster.Env
	descTTL    time.Duration
	descFlight singleflight.Group // prevents thundering herd on describe (key = c.Env)
}

// New builds an IRSA-based EKS Provider.
// Callers build awsCfg with e.g. `awsconfig.LoadDefaultConfig`.
// A nil metrics is safe (recording becomes a no-op).
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

// Type implements the auth.EndpointProvider contract.
func (p *Provider) Type() string { return config.ProviderTypeEKS }

// RestConfig builds the rest.Config for the cluster.
// The bearer token is generated per request via WrapTransport.
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

// awsConfigForCluster returns the aws.Config for the cluster,
// switching to temporary credentials via stscreds when AssumeRoleArn is set.
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

// describe caches eks:DescribeCluster results for 5 minutes.
// On a miss, singleflight collapses parallel calls for the same cluster into one.
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
		// Re-check the cache inside the flight (a prior flight may have written it)
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

// generateToken builds a bearer token with aws-iam-authenticator.
// With AssumeRoleArn, the authenticator performs the AssumeRole.
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

// tokenRoundTripper is a transport wrapper that generates a bearer token per request.
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

// Type assertions
var _ http.RoundTripper = (*tokenRoundTripper)(nil)
var _ tokenGenerator = (tokenpkg.Generator)(nil)
