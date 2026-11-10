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

func TestManager_DeleteCluster_DropsOnlyThatCluster(t *testing.T) {
	t.Parallel()

	m := New()
	m.SetActivation("stg", "c1", "ns", "api", 1)
	m.SetActivation("stg", "c1", "ns", "web", 0)
	m.SetActivation("dev", "c2", "ns", "api", 1)
	m.SetReachable("c1", 1)
	m.SetReachable("c2", 1)
	m.SetExternalScalerStreams("c1", 2)
	m.SetExternalScalerStreams("c2", 1)

	m.DeleteCluster("stg", "c1")

	if got := testutil.CollectAndCount(m.activation); got != 1 {
		t.Errorf("activation series: got %d, want 1 (dev only)", got)
	}
	if got := testutil.CollectAndCount(m.reachable); got != 1 {
		t.Errorf("reachable series: got %d, want 1 (c2 only)", got)
	}
	if got := testutil.CollectAndCount(m.extStreams); got != 1 {
		t.Errorf("external scaler stream series: got %d, want 1 (c2 only)", got)
	}
}

func TestManager_IncConfigReload(t *testing.T) {
	t.Parallel()

	m := New()
	m.IncConfigReload("registry", "error")
	m.IncConfigReload("registry", "error")

	if got := testutil.ToFloat64(m.configReload.WithLabelValues("registry", "error")); got != 2 {
		t.Errorf("registry/error = %v, want 2", got)
	}
	if got := testutil.CollectAndCount(m.configReload); got != 4 {
		t.Errorf("series = %d, want 4 pre-created", got)
	}
}

func TestManager_IncReconcile(t *testing.T) {
	t.Parallel()

	m := New()
	m.IncReconcile("c1", "error")
	m.IncReconcile("c1", "error")
	m.IncReconcile("c1", "success")

	if got := testutil.ToFloat64(m.reconcile.WithLabelValues("c1", "error")); got != 2 {
		t.Errorf("c1/error = %v, want 2", got)
	}
	if got := testutil.CollectAndCount(m.reconcile); got != 2 {
		t.Errorf("series = %d, want 2", got)
	}
}

func TestManager_IncAutoStop(t *testing.T) {
	t.Parallel()

	m := New()
	m.IncAutoStop("stg", "c1", "success")
	m.IncAutoStop("stg", "c1", "success")
	m.IncAutoStop("stg", "c1", "error")

	if got := testutil.ToFloat64(m.autoStops.WithLabelValues("stg", "c1", "success")); got != 2 {
		t.Errorf("success = %v, want 2", got)
	}
	if got := testutil.CollectAndCount(m.autoStops); got != 2 {
		t.Errorf("series = %d, want 2", got)
	}
}
