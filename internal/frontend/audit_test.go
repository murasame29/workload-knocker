package frontend

import (
	"context"
	"sync"
	"testing"

	"github.com/murasame29/workload-knocker/internal/audit"
	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/mode"
	"github.com/murasame29/workload-knocker/internal/model"
)

type recorderFake struct {
	mu   sync.Mutex
	recs []audit.Record
}

func (f *recorderFake) Record(_ context.Context, r audit.Record) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, r)
}

func TestService_Apply_AuditEvents(t *testing.T) {
	t.Parallel()

	cl := config.Cluster{Env: "stg", Name: "flowers"}
	subjects := []struct {
		sub    authz.Subject
		source audit.Source
		user   string
	}{
		{authz.Subject{Platform: authz.PlatformSlack, User: "U1"}, audit.SourceSlack, "slack:U1"},
		{authz.Subject{Platform: authz.PlatformDiscord, User: "123"}, audit.SourceDiscord, "discord:123"},
	}
	tests := []struct {
		name        string
		allow       authz.Authorizer
		mode        mode.Mode
		start, stop []string
		want        []audit.Record // Source / EndUser / RequestID are filled per subject
	}{
		{"start success", allowAll, mode.Full, []string{"app"}, nil,
			[]audit.Record{{Event: audit.WorkloadStart, Outcome: audit.Success, WorkloadName: "app", WorkloadKind: "Deployment"}}},
		{"stop success", allowAll, mode.Full, nil, []string{"app"},
			[]audit.Record{{Event: audit.WorkloadStop, Outcome: audit.Success, WorkloadName: "app", WorkloadKind: "Deployment"}}},
		{"start not found is failure", allowAll, mode.Full, []string{"missing"}, nil,
			[]audit.Record{{Event: audit.WorkloadStart, Outcome: audit.Failure, WorkloadName: "missing", Err: errNotFound}}},
		{"policy denied", authzFunc(func(authz.Subject, string, string, string, authz.Action) bool { return false }), mode.Full, []string{"app"}, nil,
			[]audit.Record{{Event: audit.WorkloadStart, Outcome: audit.Denied, WorkloadName: "app", WorkloadKind: "Deployment"}}},
		{"mode denied", allowAll, mode.ScheduledStop, nil, []string{"app"},
			[]audit.Record{{Event: audit.WorkloadStop, Outcome: audit.Denied, WorkloadName: "app", WorkloadKind: "Deployment"}}},
	}
	for _, tt := range tests {
		for _, s := range subjects {
			t.Run(tt.name+"/"+string(s.source), func(t *testing.T) {
				t.Parallel()
				rec := &recorderFake{}
				svc := NewService(&fakeStore{workloads: []*model.Workload{wl("app", 1)}}, metrics.New(), 0,
					WithAuthorizer(tt.allow), WithMode(tt.mode), WithRecorder(rec))

				svc.Apply(ApplyRequest{Cluster: cl, Namespace: "labs", User: "display", Subject: s.sub, RequestID: "rid-1", ToStart: tt.start, ToStop: tt.stop})

				if len(rec.recs) != len(tt.want) {
					t.Fatalf("got %d records, want %d: %+v", len(rec.recs), len(tt.want), rec.recs)
				}
				for i, w := range tt.want {
					w.Source, w.EndUser, w.RequestID = s.source, s.user, "rid-1"
					w.Cluster, w.Namespace = "flowers", "labs"
					if got := rec.recs[i]; got != w {
						t.Errorf("record %d:\n got  %+v\n want %+v", i, got, w)
					}
				}
			})
		}
	}
}

func TestService_AuditRequestIDTiesPreviewAndApply(t *testing.T) {
	t.Parallel()

	rec := &recorderFake{}
	denyLegacy := authzFunc(func(_ authz.Subject, _, _, name string, _ authz.Action) bool { return name != "legacy" })
	svc := NewService(&fakeStore{}, metrics.New(), 0, WithAuthorizer(denyLegacy), WithRecorder(rec))
	cl := config.Cluster{Env: "stg", Name: "c"}
	entries := GroupEntries([]*model.Workload{wl("ok", 0), wl("legacy", 0)})

	plan := svc.Plan(alice, cl, "labs", entries, map[string]bool{"ok": true, "legacy": true}, "rid-2")
	svc.Apply(ApplyRequest{Cluster: cl, Namespace: "labs", Subject: alice, RequestID: "rid-2", ToStart: plan.ToStart})

	if len(rec.recs) != 2 {
		t.Fatalf("records = %+v", rec.recs)
	}
	if rec.recs[0].Outcome != audit.Denied || rec.recs[0].WorkloadName != "legacy" || rec.recs[1].Outcome != audit.Success || rec.recs[1].WorkloadName != "ok" {
		t.Errorf("records = %+v", rec.recs)
	}
	for _, r := range rec.recs {
		if r.RequestID != "rid-2" {
			t.Errorf("request id = %q, want rid-2", r.RequestID)
		}
	}
}

func TestService_ApplyGeneratesRequestIDWhenEmpty(t *testing.T) {
	t.Parallel()

	rec := &recorderFake{}
	svc := NewService(&fakeStore{}, metrics.New(), 0, WithAuthorizer(allowAll), WithRecorder(rec))
	svc.Apply(ApplyRequest{Cluster: config.Cluster{Env: "stg"}, Namespace: "labs", Subject: alice, ToStart: []string{"a", "b"}})

	if len(rec.recs) != 2 || rec.recs[0].RequestID == "" || rec.recs[0].RequestID != rec.recs[1].RequestID {
		t.Errorf("records = %+v", rec.recs)
	}
}
