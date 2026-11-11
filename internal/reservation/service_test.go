package reservation

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/murasame29/workload-knocker/internal/audit"
	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/k8s"
	"github.com/murasame29/workload-knocker/internal/model"
	"github.com/murasame29/workload-knocker/internal/notify"
	"github.com/murasame29/workload-knocker/internal/runlimit"
)

var t0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

type memKeyStore struct {
	mu   sync.Mutex
	data string
	fail error
}

func (m *memKeyStore) Load(context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data, nil
}

func (m *memKeyStore) Save(_ context.Context, v string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.data = v
	return nil
}

type fakeStore struct {
	mu        sync.Mutex
	workloads []*model.Workload
	keys      *memKeyStore
	starts    []string
	stopAts   map[string]time.Time
	refuse    map[string]bool
}

func (f *fakeStore) Clusters() []model.ClusterRef {
	return []model.ClusterRef{{Env: "stg", Name: "stg-cluster"}}
}
func (f *fakeStore) WorkloadsByNamespace(env, ns string) []*model.Workload {
	if env != "stg" {
		return nil
	}
	var out []*model.Workload
	for _, w := range f.workloads {
		if w.Namespace == ns {
			c := *w
			out = append(out, &c)
		}
	}
	return out
}
func (f *fakeStore) Start(_, _, name, user string, stopAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuse[name] {
		return &model.RefusedError{Reason: "replicas is managed by argocd-controller"}
	}
	f.starts = append(f.starts, name+":"+user)
	if f.stopAts == nil {
		f.stopAts = map[string]time.Time{}
	}
	f.stopAts[name] = stopAt
	return nil
}
func (f *fakeStore) ReservationStore(env string) (k8s.KeyStore, bool) {
	if env != "stg" {
		return nil, false
	}
	return f.keys, true
}

type fakeNotifier struct {
	mu        sync.Mutex
	started   []notify.Event
	announced []string
}

func (n *fakeNotifier) Started(_ context.Context, e notify.Event) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.started = append(n.started, e)
}
func (n *fakeNotifier) Announce(_ context.Context, _, text string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.announced = append(n.announced, text)
}

// authzFunc adapts a function to authz.Authorizer.
type authzFunc func(authz.Subject, string, string, string, authz.Action) bool

func (f authzFunc) Allowed(s authz.Subject, env, ns, w string, a authz.Action) bool {
	return f(s, env, ns, w, a)
}

var allowAll = authzFunc(func(authz.Subject, string, string, string, authz.Action) bool { return true })

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

type recorder struct {
	mu   sync.Mutex
	recs []audit.Record
}

func (r *recorder) Record(_ context.Context, rec audit.Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec)
}

var alice = authz.Subject{Platform: authz.PlatformDiscord, User: "100000000000000001"}

func settings() Settings {
	return Settings{Warmup: 5 * time.Minute, Grace: 15 * time.Minute, Reminder: 15 * time.Minute, Run: runlimit.Defaults{Default: 4 * time.Hour, Max: 8 * time.Hour}}
}

type env struct {
	store *fakeStore
	note  *fakeNotifier
	clk   *clock
	rec   *recorder
	svc   *Service
}

func newEnv(t *testing.T, az authz.Authorizer, ws ...*model.Workload) *env {
	t.Helper()
	e := &env{store: &fakeStore{workloads: ws, keys: &memKeyStore{}}, note: &fakeNotifier{}, clk: &clock{now: t0}, rec: &recorder{}}
	e.svc = e.restart(az)
	return e
}

// restart builds a fresh Service over the same persisted reservations (a controller restart).
func (e *env) restart(az authz.Authorizer) *Service {
	e.svc = New(e.store, az, settings(), WithNotifier(e.note), WithClock(e.clk.Now), WithRecorder(e.rec))
	return e.svc
}

func wl(name string) *model.Workload {
	return &model.Workload{Name: name, Namespace: "games", Kind: "StatefulSet"}
}

func (e *env) create(t *testing.T, at time.Time, d time.Duration, names ...string) Reservation {
	t.Helper()
	r, err := e.svc.Create(context.Background(), CreateRequest{Env: "stg", Namespace: "games", Workloads: names, At: at, Duration: d, Subject: alice, Source: audit.SourceDiscord})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return r
}

func (e *env) tick(at time.Time) {
	e.clk.set(at)
	e.svc.Tick(context.Background(), at)
}

func (e *env) state(t *testing.T, id string) Reservation {
	t.Helper()
	list, err := e.svc.List(context.Background(), "stg")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range list {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("reservation %s not listed", id)
	return Reservation{}
}

func TestCreate_Validates(t *testing.T) {
	t.Parallel()
	deny := authzFunc(func(_ authz.Subject, _, _, w string, _ authz.Action) bool { return w != "secret" })
	e := newEnv(t, deny, wl("terraria"), wl("secret"))

	cases := []struct {
		name string
		req  CreateRequest
		want error
	}{
		{"past", CreateRequest{Env: "stg", Namespace: "games", Workloads: []string{"terraria"}, At: t0.Add(-time.Minute)}, ErrInvalid},
		{"too far", CreateRequest{Env: "stg", Namespace: "games", Workloads: []string{"terraria"}, At: t0.Add(MaxAhead + time.Hour)}, ErrInvalid},
		{"no workloads", CreateRequest{Env: "stg", Namespace: "games", At: t0.Add(time.Hour)}, ErrInvalid},
		{"unknown env", CreateRequest{Env: "prod", Namespace: "games", Workloads: []string{"terraria"}, At: t0.Add(time.Hour)}, ErrNotFound},
		{"unknown workload", CreateRequest{Env: "stg", Namespace: "games", Workloads: []string{"nope"}, At: t0.Add(time.Hour)}, ErrNotFound},
		{"forbidden", CreateRequest{Env: "stg", Namespace: "games", Workloads: []string{"terraria", "secret"}, At: t0.Add(time.Hour)}, ErrForbidden},
		{"duration over max", CreateRequest{Env: "stg", Namespace: "games", Workloads: []string{"terraria"}, At: t0.Add(time.Hour), Duration: 9 * time.Hour}, ErrInvalid},
	}
	for _, tc := range cases {
		tc.req.Subject = alice
		if _, err := e.svc.Create(context.Background(), tc.req); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if e.store.keys.data != "" {
		t.Errorf("invalid requests persisted %q", e.store.keys.data)
	}
}

func TestTick_RemindsThenStartsWarmupAheadAndStopsAfterDuration(t *testing.T) {
	t.Parallel()
	e := newEnv(t, allowAll, wl("terraria"), wl("tommy-world"))
	at := t0.Add(9 * time.Hour) // 21:00
	r := e.create(t, at, 3*time.Hour, "tommy-world", "terraria")

	e.tick(at.Add(-21 * time.Minute)) // before the reminder window (start 20:55, reminder 20:40)
	if len(e.note.announced) != 0 || len(e.store.starts) != 0 {
		t.Fatalf("too early: announced %v, starts %v", e.note.announced, e.store.starts)
	}
	e.tick(at.Add(-19 * time.Minute))
	e.tick(at.Add(-18 * time.Minute))
	if len(e.note.announced) != 1 || !strings.Contains(e.note.announced[0], "terraria, tommy-world start at 20:55 UTC to be ready by 21:00 UTC") {
		t.Fatalf("reminders = %q, want one", e.note.announced)
	}

	e.tick(at.Add(-6 * time.Minute))
	if len(e.store.starts) != 0 {
		t.Fatalf("started before the warmup: %v", e.store.starts)
	}
	e.tick(at.Add(-5 * time.Minute))
	if want := []string{"terraria:" + alice.User, "tommy-world:" + alice.User}; !slices.Equal(e.store.starts, want) {
		t.Fatalf("starts = %v, want %v", e.store.starts, want)
	}
	if got := e.store.stopAts["terraria"]; !got.Equal(at.Add(3 * time.Hour)) {
		t.Errorf("stopAt = %v, want the reserved time + 3h", got)
	}
	if got := e.state(t, r.ID); got.State != Started {
		t.Errorf("state = %s, want started", got.State)
	}
	if len(e.note.started) != 1 || len(e.note.started[0].Workloads) != 2 || !strings.Contains(e.note.started[0].Actor, "reservation by <@"+alice.User+">") {
		t.Errorf("start notice = %+v", e.note.started)
	}

	e.tick(at)
	if len(e.store.starts) != 2 {
		t.Errorf("started twice: %v", e.store.starts)
	}
}

func TestTick_WarmupAnnotationStartsEarlier(t *testing.T) {
	t.Parallel()
	slow := wl("terraria")
	slow.Warmup = 20 * time.Minute
	e := newEnv(t, allowAll, slow)
	at := t0.Add(2 * time.Hour)
	e.create(t, at, 0, "terraria")

	e.tick(at.Add(-21 * time.Minute))
	if len(e.store.starts) != 0 {
		t.Fatal("started before the annotated warmup")
	}
	e.tick(at.Add(-20 * time.Minute))
	if len(e.store.starts) != 1 {
		t.Fatalf("not started at the annotated warmup: %v", e.store.starts)
	}
	if got := e.store.stopAts["terraria"]; !got.Equal(at.Add(4 * time.Hour)) {
		t.Errorf("stopAt = %v, want the default duration after the reserved time", got)
	}
}

func TestTick_SurvivesRestartAndMissesAfterGrace(t *testing.T) {
	t.Parallel()
	e := newEnv(t, allowAll, wl("a"), wl("b"))
	at := t0.Add(time.Hour)
	ra := e.create(t, at, 0, "a")
	rb := e.create(t, at.Add(time.Hour), 0, "b")

	// Restarted 10 minutes after a's start (within grace) and 30 minutes after b's (past grace).
	e.restart(allowAll)
	e.tick(at.Add(-5*time.Minute + 10*time.Minute))
	if got := e.state(t, ra.ID).State; got != Started || !slices.Equal(e.store.starts, []string{"a:" + alice.User}) {
		t.Errorf("a: state %s, starts %v, want it run within grace", got, e.store.starts)
	}

	e.restart(allowAll)
	e.tick(at.Add(time.Hour - 5*time.Minute + 30*time.Minute))
	got := e.state(t, rb.ID)
	if got.State != Missed || !strings.Contains(got.Note, "unavailable") {
		t.Errorf("b: %+v, want missed", got)
	}
	if len(e.store.starts) != 1 {
		t.Errorf("missed reservation started: %v", e.store.starts)
	}
	if n := len(e.note.announced); n == 0 || !strings.Contains(e.note.announced[n-1], "was missed") {
		t.Errorf("announced = %q, want a missed notice", e.note.announced)
	}
}

func TestTick_CrashBeforeTheResultIsSavedRunsAgainAfterRestart(t *testing.T) {
	t.Parallel()
	e := newEnv(t, allowAll, wl("terraria"))
	at := t0.Add(time.Hour)
	r := e.create(t, at, 0, "terraria")

	// The workload starts, then saving the result fails (the controller dies at that point).
	e.store.keys.fail = errors.New("controller gone")
	e.tick(at.Add(-5 * time.Minute))
	if len(e.store.starts) != 1 {
		t.Fatalf("starts = %v, want one", e.store.starts)
	}
	e.store.keys.fail = nil

	e.restart(allowAll)
	if got := e.state(t, r.ID).State; got != Pending {
		t.Fatalf("persisted state = %s, want pending (the result was never saved)", got)
	}
	e.tick(at.Add(-4 * time.Minute))
	if got := e.state(t, r.ID).State; got != Started || len(e.store.starts) != 2 {
		t.Errorf("after restart: state %s, starts %v, want started again", got, e.store.starts)
	}
	e.tick(at)
	if len(e.store.starts) != 2 {
		t.Errorf("started a third time: %v", e.store.starts)
	}
}

func TestTick_CancelDuringExecutionKeepsCancelled(t *testing.T) {
	t.Parallel()
	e := newEnv(t, allowAll, wl("terraria"))
	r := e.create(t, t0.Add(time.Hour), 0, "terraria")
	e.svc.markDone(context.Background(), "stg", r.ID, Started, "", t0)
	if got := e.state(t, r.ID).State; got != Started {
		t.Fatalf("state = %s", got)
	}
	r2 := e.create(t, t0.Add(2*time.Hour), 0, "terraria")
	if _, err := e.svc.Cancel(context.Background(), "stg", r2.ID, alice, audit.SourceDiscord); err != nil {
		t.Fatal(err)
	}
	e.svc.markDone(context.Background(), "stg", r2.ID, Started, "", t0)
	if got := e.state(t, r2.ID).State; got != Cancelled {
		t.Errorf("state = %s, want cancelled", got)
	}
}

func TestTick_RechecksAuthorizationAtExecution(t *testing.T) {
	t.Parallel()
	allowed := true
	var mu sync.Mutex
	az := authzFunc(func(authz.Subject, string, string, string, authz.Action) bool {
		mu.Lock()
		defer mu.Unlock()
		return allowed
	})
	e := newEnv(t, az, wl("terraria"))
	at := t0.Add(time.Hour)
	r := e.create(t, at, 0, "terraria")

	mu.Lock()
	allowed = false
	mu.Unlock()
	e.tick(at.Add(-5 * time.Minute))
	got := e.state(t, r.ID)
	if got.State != Failed || !strings.Contains(got.Note, "not authorized") || len(e.store.starts) != 0 {
		t.Errorf("reservation = %+v, starts %v, want failed without starting", got, e.store.starts)
	}
	var exec *audit.Record
	for i := range e.rec.recs {
		if e.rec.recs[i].Event == audit.ReservationExecute {
			exec = &e.rec.recs[i]
		}
	}
	if exec == nil || exec.Outcome != audit.Failure || exec.RequestID != r.ID {
		t.Errorf("execute audit = %+v", exec)
	}
}

func TestTick_PartialStartKeepsNote(t *testing.T) {
	t.Parallel()
	e := newEnv(t, allowAll, wl("a"), wl("b"))
	e.store.refuse = map[string]bool{"b": true}
	at := t0.Add(time.Hour)
	r := e.create(t, at, 0, "a", "b")

	e.tick(at.Add(-5 * time.Minute))
	got := e.state(t, r.ID)
	if got.State != Started || !strings.Contains(got.Note, "b: replicas is managed by argocd-controller") {
		t.Errorf("reservation = %+v, want started with the refusal noted", got)
	}
}

func TestCancel_CreatorOrStopPermission(t *testing.T) {
	t.Parallel()
	bob := authz.Subject{Platform: authz.PlatformDiscord, User: "100000000000000002"}
	admin := authz.Subject{Platform: authz.PlatformDiscord, User: "100000000000000003"}
	az := authzFunc(func(s authz.Subject, _, _, _ string, a authz.Action) bool {
		return s.User != bob.User || a == authz.ActionStart
	})
	e := newEnv(t, az, wl("terraria"))
	r1 := e.create(t, t0.Add(time.Hour), 0, "terraria")
	r2 := e.create(t, t0.Add(2*time.Hour), 0, "terraria")

	if _, err := e.svc.Cancel(context.Background(), "stg", r1.ID, bob, audit.SourceDiscord); !errors.Is(err, ErrForbidden) {
		t.Errorf("bob cancel = %v, want forbidden", err)
	}
	if got := e.svc.Visible(context.Background(), bob, "stg"); len(got) != 0 {
		t.Errorf("bob sees %v", got)
	}
	if _, err := e.svc.Cancel(context.Background(), "stg", r1.ID, alice, audit.SourceDiscord); err != nil {
		t.Errorf("creator cancel: %v", err)
	}
	if _, err := e.svc.Cancel(context.Background(), "stg", r2.ID, admin, audit.SourceAPI); err != nil {
		t.Errorf("admin cancel: %v", err)
	}
	if _, err := e.svc.Cancel(context.Background(), "stg", r2.ID, admin, audit.SourceAPI); !errors.Is(err, ErrNotFound) {
		t.Errorf("second cancel = %v, want not found", err)
	}
	e.tick(t0.Add(2 * time.Hour))
	if len(e.store.starts) != 0 {
		t.Errorf("cancelled reservations started: %v", e.store.starts)
	}
}

// countingKeyStore counts saves.
type countingKeyStore struct {
	memKeyStore
	saves int
}

func (c *countingKeyStore) Save(ctx context.Context, v string) error {
	c.saves++
	return c.memKeyStore.Save(ctx, v)
}

func TestTick_SavesOnlyOnChange(t *testing.T) {
	t.Parallel()
	ks := &countingKeyStore{}
	st := &fakeStore{workloads: []*model.Workload{wl("terraria")}}
	svc := New(&countingStore{fakeStore: st, ks: ks}, allowAll, settings(), WithClock(func() time.Time { return t0 }))
	for i := range 3 {
		svc.Tick(context.Background(), t0.Add(time.Duration(i)*time.Minute))
	}
	if ks.saves != 0 {
		t.Errorf("idle ticks saved %d times", ks.saves)
	}
	if _, err := svc.Create(context.Background(), CreateRequest{Env: "stg", Namespace: "games", Workloads: []string{"terraria"}, At: t0.Add(5 * time.Hour), Subject: alice}); err != nil {
		t.Fatal(err)
	}
	svc.Tick(context.Background(), t0.Add(time.Minute))
	if ks.saves != 1 {
		t.Errorf("saves = %d, want only the create", ks.saves)
	}
}

// countingStore serves a countingKeyStore as the reservation store.
type countingStore struct {
	*fakeStore
	ks *countingKeyStore
}

func (c *countingStore) ReservationStore(string) (k8s.KeyStore, bool) { return c.ks, true }

func TestPersistence_FailedSaveKeepsMemoryAndPrunesAfterRetention(t *testing.T) {
	t.Parallel()
	e := newEnv(t, allowAll, wl("terraria"))
	r := e.create(t, t0.Add(time.Hour), 0, "terraria")

	var saved []Reservation
	if err := json.Unmarshal([]byte(e.store.keys.data), &saved); err != nil || len(saved) != 1 || saved[0].ID != r.ID {
		t.Fatalf("persisted %q: %v", e.store.keys.data, err)
	}

	e.store.keys.fail = errors.New("api down")
	if _, err := e.svc.Create(context.Background(), CreateRequest{Env: "stg", Namespace: "games", Workloads: []string{"terraria"}, At: t0.Add(2 * time.Hour), Subject: alice}); err == nil {
		t.Fatal("Create succeeded although the save failed")
	}
	if list, _ := e.svc.List(context.Background(), "stg"); len(list) != 1 {
		t.Errorf("memory diverged from the ConfigMap: %d reservations", len(list))
	}
	e.store.keys.fail = nil

	e.tick(t0.Add(55 * time.Minute))
	e.tick(t0.Add(55*time.Minute + retention + time.Minute))
	if list, _ := e.svc.List(context.Background(), "stg"); len(list) != 0 {
		t.Errorf("finished reservation kept past retention: %+v", list)
	}
}
