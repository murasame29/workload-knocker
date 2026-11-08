package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestManager_ObserveOperation_CountsByLabels(t *testing.T) {
	t.Parallel()

	m := New()
	m.ObserveOperation("stg", "c1", "start", "success", 50*time.Millisecond)
	m.ObserveOperation("stg", "c1", "start", "success", 10*time.Millisecond)
	m.ObserveOperation("stg", "c1", "start", "not_found", time.Millisecond)
	m.ObserveOperation("stg", "c1", "stop_all", "error", time.Second)

	want := `
# HELP workload_knocker_operations_total Total number of workload operations, labeled by env, cluster, operation (start, stop, stop_all) and result (success, error, not_found)
# TYPE workload_knocker_operations_total counter
workload_knocker_operations_total{cluster="c1",env="stg",operation="start",result="not_found"} 1
workload_knocker_operations_total{cluster="c1",env="stg",operation="start",result="success"} 2
workload_knocker_operations_total{cluster="c1",env="stg",operation="stop_all",result="error"} 1
`
	if err := testutil.GatherAndCompare(m.registry, strings.NewReader(want), "workload_knocker_operations_total"); err != nil {
		t.Fatal(err)
	}
}

func TestManager_ObserveOperation_RecordsDuration(t *testing.T) {
	t.Parallel()

	m := New()
	m.ObserveOperation("stg", "c1", "stop", "success", 20*time.Millisecond)
	m.ObserveOperation("stg", "c1", "stop", "error", 30*time.Millisecond)

	// One histogram series per env/cluster/operation, regardless of result.
	if got := testutil.CollectAndCount(m.opDuration, "workload_knocker_operation_duration_seconds"); got != 1 {
		t.Fatalf("series count: got %d, want 1", got)
	}
}

func TestManager_ExistingMetricsUnchanged(t *testing.T) {
	t.Parallel()

	m := New()
	m.SetActivation("stg", "c1", "ns", "api", 1)
	m.SetReachable("c1", 1)
	m.IncTokenRefresh("c1", "success")

	if got := testutil.CollectAndCount(m.activation); got != 1 {
		t.Fatalf("activation series: got %d, want 1", got)
	}
	if got := testutil.ToFloat64(m.tokenRefresh.WithLabelValues("c1", "success")); got != 1 {
		t.Fatalf("token refresh: got %v, want 1", got)
	}
}
