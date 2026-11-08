package eks

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	tokenpkg "sigs.k8s.io/aws-iam-authenticator/pkg/token"

	"github.com/murasame29/workload-knocker/internal/config"
)

// TestNew_Construction は Provider のコンストラクタが空の aws.Config でも nil でない実装を返すことを確認する。
func TestNew_Construction(t *testing.T) {
	t.Parallel()

	p, err := New(aws.Config{}, nil)
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("New: returned nil")
	}
	if p.gen == nil {
		t.Error("Provider.gen should not be nil after construction")
	}
	if p.descCache == nil {
		t.Error("Provider.descCache should be initialized")
	}
	if p.descTTL <= 0 {
		t.Errorf("Provider.descTTL should be positive, got %v", p.descTTL)
	}
	if got := p.Type(); got != config.ProviderTypeEKS {
		t.Errorf("Type(): got %q, want %q", got, config.ProviderTypeEKS)
	}
}

// fakeTokenGenerator は tokenGenerator interface の test stub。
type fakeTokenGenerator struct {
	token string
	err   error
}

func (f *fakeTokenGenerator) GetWithOptions(_ context.Context, _ *tokenpkg.GetTokenOptions) (tokenpkg.Token, error) {
	if f.err != nil {
		return tokenpkg.Token{}, f.err
	}
	return tokenpkg.Token{
		Token:      f.token,
		Expiration: time.Now().Add(15 * time.Minute),
	}, nil
}

// fakeMetrics は MetricsHook の test stub。呼び出し回数を記録する。
type fakeMetrics struct {
	mu              sync.Mutex
	tokenSuccesses  int
	tokenErrors     int
	tokenRefreshSeq []tokenRefreshCall
}

type tokenRefreshCall struct {
	cluster string
	result  string
}

func (m *fakeMetrics) IncTokenRefresh(cluster, result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokenRefreshSeq = append(m.tokenRefreshSeq, tokenRefreshCall{cluster, result})
	switch result {
	case "success":
		m.tokenSuccesses++
	case "error":
		m.tokenErrors++
	}
}

// TestTokenRoundTripper_AddsAuthorizationHeader は tokenRoundTripper が
// Authorization: Bearer <token> ヘッダを各リクエストに付与し、
// IncTokenRefresh(success) を記録することを検証する。
// metrics の cluster ラベル値は config.Cluster.Name であることも併せて確認する。
func TestTokenRoundTripper_AddsAuthorizationHeader(t *testing.T) {
	t.Parallel()

	const wantToken = "k8s-aws-v1.fake-token"
	var (
		gotAuthHeader string
		gotHeaderMu   sync.Mutex
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaderMu.Lock()
		gotAuthHeader = r.Header.Get("Authorization")
		gotHeaderMu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)

	metrics := &fakeMetrics{}
	provider := &Provider{
		awsCfg:    aws.Config{},
		metrics:   metrics,
		gen:       &fakeTokenGenerator{token: wantToken},
		descCache: make(map[string]describeCacheEntry),
		descTTL:   5 * time.Minute,
	}

	c := config.Cluster{
		Env:  "test",
		Name: "test-cluster",
		Provider: config.ClusterProvider{
			Type: config.ProviderTypeEKS,
			EKS:  &config.ProviderEKS{Region: "ap-northeast-1"},
		},
	}

	rt := &tokenRoundTripper{
		base:     http.DefaultTransport,
		provider: provider,
		cluster:  c,
	}
	client := &http.Client{Transport: rt, Timeout: 5 * time.Second}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: want 200, got %d", resp.StatusCode)
	}

	gotHeaderMu.Lock()
	defer gotHeaderMu.Unlock()
	wantHeader := "Bearer " + wantToken
	if gotAuthHeader != wantHeader {
		t.Errorf("Authorization header mismatch: want %q, got %q", wantHeader, gotAuthHeader)
	}

	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	if metrics.tokenSuccesses != 1 {
		t.Errorf("tokenSuccesses: want 1, got %d", metrics.tokenSuccesses)
	}
	if metrics.tokenErrors != 0 {
		t.Errorf("tokenErrors: want 0, got %d", metrics.tokenErrors)
	}
	if len(metrics.tokenRefreshSeq) != 1 {
		t.Fatalf("tokenRefreshSeq: want 1 entry, got %d", len(metrics.tokenRefreshSeq))
	}
	if got := metrics.tokenRefreshSeq[0]; got.cluster != "test-cluster" || got.result != "success" {
		t.Errorf("tokenRefreshSeq[0]: want {test-cluster, success}, got {%s, %s}", got.cluster, got.result)
	}
}

// TestTokenRoundTripper_RecordsErrorOnFailure は token 生成失敗時に
// IncTokenRefresh(error) を記録し、エラーが伝播することを検証する。
func TestTokenRoundTripper_RecordsErrorOnFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("server should not be reached when token generation fails")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	metrics := &fakeMetrics{}
	provider := &Provider{
		awsCfg:    aws.Config{},
		metrics:   metrics,
		gen:       &fakeTokenGenerator{err: errStub("boom")},
		descCache: make(map[string]describeCacheEntry),
		descTTL:   5 * time.Minute,
	}

	c := config.Cluster{
		Env:  "broken",
		Name: "broken-cluster",
		Provider: config.ClusterProvider{
			Type: config.ProviderTypeEKS,
			EKS:  &config.ProviderEKS{Region: "ap-northeast-1"},
		},
	}

	rt := &tokenRoundTripper{
		base:     http.DefaultTransport,
		provider: provider,
		cluster:  c,
	}
	client := &http.Client{Transport: rt, Timeout: 5 * time.Second}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	if _, err := client.Do(req); err == nil {
		t.Fatal("client.Do: expected error, got nil")
	}

	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	if metrics.tokenErrors != 1 {
		t.Errorf("tokenErrors: want 1, got %d", metrics.tokenErrors)
	}
	if metrics.tokenSuccesses != 0 {
		t.Errorf("tokenSuccesses: want 0, got %d", metrics.tokenSuccesses)
	}
}

// errStub は簡易エラー型 (テスト用)。
type errStub string

func (e errStub) Error() string { return string(e) }
