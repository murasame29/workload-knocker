package kedaexternal

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	pb "github.com/kedacore/keda/v2/pkg/scalers/externalscaler"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/murasame29/workload-knocker/internal/actuator"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/metrics"
)

const wait = 5 * time.Second

type streamGauge struct {
	metrics.MetricsManager
	mu sync.Mutex
	n  map[string]int
}

func (g *streamGauge) SetExternalScalerStreams(cluster string, n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n[cluster] = n
}

func (g *streamGauge) get(cluster string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n[cluster]
}

// startKEDA serves hub on an in-process listener and returns a client that plays KEDA.
func startKEDA(t *testing.T, hub *Hub) pb.ExternalScalerClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pb.RegisterExternalScalerServer(srv, hub)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pb.NewExternalScalerClient(conn)
}

func ref(md map[string]string) *pb.ScaledObjectRef {
	return &pb.ScaledObjectRef{Name: "so", Namespace: "ns", ScalerMetadata: md}
}

var explicitMD = map[string]string{"cluster": "stg", "workload": "app"}

func newTestHub(m metrics.MetricsManager) *Hub {
	hub := NewHub()
	hub.keepalive = time.Hour
	hub.register(config.Cluster{Env: "stg", Name: "stg-cluster"}, m)
	return hub
}

func recv(t *testing.T, s grpc.ServerStreamingClient[pb.IsActiveResponse]) bool {
	t.Helper()
	type result struct {
		resp *pb.IsActiveResponse
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		r, err := s.Recv()
		ch <- result{r, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("Recv: %v", r.err)
		}
		return r.resp.Result
	case <-time.After(wait):
		t.Fatal("no push received")
		return false
	}
}

func TestIsActiveAndGetMetrics_FollowTheDesiredState(t *testing.T) {
	t.Parallel()

	hub := newTestHub(nil)
	c := startKEDA(t, hub)
	ctx := t.Context()

	for _, tc := range []struct {
		running bool
		value   float64
	}{{true, 1}, {false, 0}} {
		hub.setRunning("stg", key{"ns", "app"}, tc.running)
		active, err := c.IsActive(ctx, ref(explicitMD))
		if err != nil || active.Result != tc.running {
			t.Fatalf("IsActive = %v, %v, want %v", active, err, tc.running)
		}
		got, err := c.GetMetrics(ctx, &pb.GetMetricsRequest{ScaledObjectRef: ref(explicitMD), MetricName: "s0-x"})
		if err != nil || len(got.MetricValues) != 1 || got.MetricValues[0].MetricValueFloat != tc.value || got.MetricValues[0].MetricName != "s0-x" {
			t.Fatalf("GetMetrics = %v, %v, want value %v", got, err, tc.value)
		}
	}
}

func TestGetMetricSpec_TargetIsOne(t *testing.T) {
	t.Parallel()

	c := startKEDA(t, newTestHub(nil))
	got, err := c.GetMetricSpec(t.Context(), ref(explicitMD))
	if err != nil || len(got.MetricSpecs) != 1 || got.MetricSpecs[0].TargetSizeFloat != 1 || got.MetricSpecs[0].MetricName == "" {
		t.Fatalf("GetMetricSpec = %v, %v", got, err)
	}
}

func TestMetadataErrors(t *testing.T) {
	t.Parallel()

	c := startKEDA(t, newTestHub(nil))
	ctx := t.Context()

	if _, err := c.IsActive(ctx, ref(map[string]string{"workload": "app"})); status.Code(err) != codes.InvalidArgument {
		t.Errorf("missing cluster: code = %v, want InvalidArgument", status.Code(err))
	}
	if _, err := c.IsActive(ctx, ref(map[string]string{"cluster": "nope", "workload": "app"})); status.Code(err) != codes.NotFound {
		t.Errorf("unknown cluster: code = %v, want NotFound", status.Code(err))
	}
	if _, err := c.IsActive(ctx, ref(explicitMD)); status.Code(err) != codes.NotFound {
		t.Errorf("workload without state: code = %v, want NotFound", status.Code(err))
	}
}

func TestIsActive_ResolvesTheScaledObjectWithoutExplicitWorkload(t *testing.T) {
	t.Parallel()

	hub := newTestHub(nil)
	c := startKEDA(t, hub)
	md := map[string]string{"cluster": "stg"}

	hub.setAliases("stg", map[key]key{{"ns", "so"}: {"ns", "deploy"}})
	hub.setRunning("stg", key{"ns", "deploy"}, true)

	active, err := c.IsActive(t.Context(), ref(md))
	if err != nil || !active.Result {
		t.Fatalf("IsActive = %v, %v, want active", active, err)
	}
}

func applyTo(hub *Hub, running bool) error {
	be := &Backend{cluster: config.Cluster{Env: "stg"}, hub: hub}
	return be.Apply(context.Background(), actuator.Workload{Namespace: "ns", Name: "app"}, running)
}

func TestStreamIsActive_PushesStartAndStop(t *testing.T) {
	t.Parallel()

	hub := newTestHub(nil)
	hub.setRunning("stg", key{"ns", "app"}, false)
	c := startKEDA(t, hub)

	s, err := c.StreamIsActive(t.Context(), ref(explicitMD))
	if err != nil {
		t.Fatal(err)
	}
	if recv(t, s) {
		t.Fatal("initial push = active, want stopped")
	}
	if err := applyTo(hub, true); err != nil {
		t.Fatal(err)
	}
	if !recv(t, s) {
		t.Error("push after start = stopped, want active")
	}
	if err := applyTo(hub, false); err != nil {
		t.Fatal(err)
	}
	if recv(t, s) {
		t.Error("push after stop = active, want stopped")
	}
}

func TestStreamIsActive_KeepaliveRepeatsTheState(t *testing.T) {
	t.Parallel()

	hub := newTestHub(nil)
	hub.keepalive = 20 * time.Millisecond
	hub.setRunning("stg", key{"ns", "app"}, true)
	c := startKEDA(t, hub)

	s, err := c.StreamIsActive(t.Context(), ref(explicitMD))
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if !recv(t, s) {
			t.Fatal("keepalive push = stopped, want active")
		}
	}
}

func TestStreamIsActive_WaitsForTheWorkloadState(t *testing.T) {
	t.Parallel()

	hub := newTestHub(nil)
	c := startKEDA(t, hub)
	s, err := c.StreamIsActive(t.Context(), ref(explicitMD))
	if err != nil {
		t.Fatal(err)
	}
	hub.setRunning("stg", key{"ns", "app"}, true)
	if !recv(t, s) {
		t.Error("push = stopped, want active")
	}
}

func TestStreamIsActive_RejectsUnknownCluster(t *testing.T) {
	t.Parallel()

	c := startKEDA(t, newTestHub(nil))
	s, err := c.StreamIsActive(t.Context(), ref(map[string]string{"cluster": "nope", "workload": "app"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recv(); status.Code(err) != codes.NotFound {
		t.Errorf("Recv code = %v, want NotFound", status.Code(err))
	}
}

func TestStreamIsActive_DisconnectReleasesTheStream(t *testing.T) {
	t.Parallel()

	gauge := &streamGauge{n: map[string]int{}}
	hub := newTestHub(gauge)
	hub.setRunning("stg", key{"ns", "app"}, true)
	c := startKEDA(t, hub)

	ctx, cancel := context.WithCancel(t.Context())
	s, err := c.StreamIsActive(ctx, ref(explicitMD))
	if err != nil {
		t.Fatal(err)
	}
	recv(t, s)
	if got := gauge.get("stg-cluster"); got != 1 {
		t.Fatalf("streams gauge = %d, want 1", got)
	}
	cancel()
	waitFor(t, func() bool { return gauge.get("stg-cluster") == 0 })
}

func TestStreamIsActive_CloseEndsStreamsWithUnavailable(t *testing.T) {
	t.Parallel()

	hub := newTestHub(nil)
	hub.setRunning("stg", key{"ns", "app"}, true)
	c := startKEDA(t, hub)
	s, err := c.StreamIsActive(t.Context(), ref(explicitMD))
	if err != nil {
		t.Fatal(err)
	}
	recv(t, s)
	hub.Close()
	hub.Close()
	if _, err := s.Recv(); status.Code(err) != codes.Unavailable {
		t.Errorf("Recv code = %v, want Unavailable", status.Code(err))
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
