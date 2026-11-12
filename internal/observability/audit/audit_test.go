package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"regexp"
	"testing"
	"time"
)

// capture swaps the default logger for a JSON logger writing to the returned buffer.
// Tests using it must not run in parallel.
func capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestSlogRecorder_ExactAttributes(t *testing.T) {
	base := Record{
		Event: WorkloadStart, RequestID: "rid", Cluster: "flowers", Namespace: "terraria",
		WorkloadName: "terraria", WorkloadKind: "StatefulSet",
	}
	tests := []struct {
		name    string
		mod     func(*Record)
		level   string
		want    map[string]any
		wantErr bool
	}{
		{"discord success", func(r *Record) { r.Outcome, r.Source, r.EndUser = Success, SourceDiscord, "discord:123" }, "INFO",
			map[string]any{"event.outcome": "success", "workload_knocker.source": "discord", "enduser.id": "discord:123"}, false},
		{"slack denied", func(r *Record) { r.Outcome, r.Source, r.EndUser = Denied, SourceSlack, "slack:U1" }, "INFO",
			map[string]any{"event.outcome": "denied", "workload_knocker.source": "slack", "enduser.id": "slack:U1"}, false},
		{"api failure", func(r *Record) {
			r.Outcome, r.Source, r.EndUser, r.Err = Failure, SourceAPI, EndUserAPI, errors.New("boom")
		}, "WARN",
			map[string]any{"event.outcome": "failure", "workload_knocker.source": "api", "enduser.id": "api", "error.type": "*errors.errorString", "error.message": "boom"}, true},
		{"nightly stop_all with schedule fields", func(r *Record) {
			r.Event, r.Outcome, r.Source, r.EndUser = WorkloadStopAll, Success, SourceNightlyStop, EndUserAPI
			r.Duration, r.StopAt = 3*time.Hour, time.Date(2026, 11, 8, 15, 0, 0, 0, time.UTC)
		}, "INFO",
			map[string]any{"event.name": "workload.stop_all", "event.outcome": "success", "workload_knocker.source": "nightly-stop", "enduser.id": "api",
				"workload_knocker.duration_seconds": float64(10800), "workload_knocker.stop_at": "2026-11-08T15:00:00Z"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := capture(t)
			r := base
			tt.mod(&r)
			NewSlog().Record(context.Background(), r)

			var got map[string]any
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal %q: %v", buf.String(), err)
			}
			want := map[string]any{
				"time": got["time"], "level": tt.level, "msg": Message,
				"event.name": "workload.start", "workload_knocker.request_id": "rid",
				"k8s.cluster.name": "flowers", "k8s.namespace.name": "terraria",
				"workload.name": "terraria", "workload.kind": "StatefulSet",
			}
			for k, v := range tt.want {
				want[k] = v
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got  %v\nwant %v", got, want)
			}
		})
	}
}

func TestSlogRecorder_OmitsEmptyFields(t *testing.T) {
	buf := capture(t)
	NewSlog().Record(context.Background(), Record{Event: WorkloadStop, Outcome: Denied, Source: SourceAPI, RequestID: "r", EndUser: EndUserAPI})
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"k8s.cluster.name", "k8s.namespace.name", "workload.name", "workload.kind", "workload_knocker.duration_seconds", "workload_knocker.stop_at", "error.type"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s should be omitted: %v", k, got)
		}
	}
}

func TestEndUser(t *testing.T) {
	t.Parallel()
	if got := EndUser(SourceSlack, "U1"); got != "slack:U1" {
		t.Errorf("got %q", got)
	}
	if got := EndUser(SourceAPI, ""); got != "api" {
		t.Errorf("got %q", got)
	}
}

func TestNewRequestID(t *testing.T) {
	t.Parallel()
	a, b := NewRequestID(), NewRequestID()
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(a) || a == b {
		t.Errorf("ids %q %q", a, b)
	}
}
