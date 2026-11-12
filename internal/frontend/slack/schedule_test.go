package slackbot

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/murasame29/workload-knocker/internal/observability/audit"
	"github.com/murasame29/workload-knocker/internal/policy/authz"
	"github.com/murasame29/workload-knocker/internal/schedule/reservation"
	"github.com/murasame29/workload-knocker/internal/workload/model"
)

type fakeReservations struct {
	mu       sync.Mutex
	created  []reservation.CreateRequest
	err      error
	list     []reservation.Reservation
	canceled []string
}

func (f *fakeReservations) Create(_ context.Context, req reservation.CreateRequest) (reservation.Reservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return reservation.Reservation{}, f.err
	}
	f.created = append(f.created, req)
	return reservation.Reservation{ID: fmt.Sprintf("r%d", len(f.created)), Namespace: req.Namespace, Workloads: req.Workloads, At: req.At, Duration: req.Duration}, nil
}

func (f *fakeReservations) Cancel(_ context.Context, _, id string, _ authz.Subject, src audit.Source) (reservation.Reservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if src != audit.SourceSlack {
		return reservation.Reservation{}, fmt.Errorf("source %s", src)
	}
	for i, r := range f.list {
		if r.ID == id {
			f.canceled = append(f.canceled, id)
			f.list = slices.Delete(f.list, i, i+1)
			return r, nil
		}
	}
	return reservation.Reservation{}, reservation.ErrNotFound
}

func (f *fakeReservations) Visible(context.Context, authz.Subject, string) []reservation.Reservation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.list)
}

func (f *fakeReservations) Warmup(string, string, []string) time.Duration { return 5 * time.Minute }

func scheduleValues(at int64, hours string, sel map[string][]string) map[string]map[string]slack.BlockAction {
	v := checked(sel)
	v[blockReserveAt] = map[string]slack.BlockAction{actionReserveAt: {SelectedDateTime: at}}
	v[blockReserveHours] = map[string]slack.BlockAction{actionReserveHours: {Value: hours}}
	return v
}

func TestParseSchedule(t *testing.T) {
	t.Parallel()
	meta := scheduleMetadata{Namespaces: []string{"alpha", "beta"}}
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

	in, errs := parseSchedule(meta, scheduleValues(at.Unix(), "3", map[string][]string{"block_ns_1_0": {"beta-w00"}, "block_ns_9_0": {"x"}}))
	if len(errs) != 0 || !in.at.Equal(at) || in.hours != 3*time.Hour || len(in.names) != 1 || !slices.Equal(in.names["beta"], []string{"beta-w00"}) {
		t.Errorf("parsed = %+v %v", in, errs)
	}
	if _, errs := parseSchedule(meta, scheduleValues(0, "x", map[string][]string{"block_ns_0_0": {"a"}})); errs[blockReserveAt] == "" || errs[blockReserveHours] == "" {
		t.Errorf("errs = %v, want time and hours errors", errs)
	}
	if _, errs := parseSchedule(meta, scheduleValues(at.Unix(), "", nil)); !strings.Contains(errs[blockReserveAt], "at least one workload") {
		t.Errorf("errs = %v, want a missing selection error", errs)
	}
}

func TestScheduleModal_UncheckedGroupsPickerAndHours(t *testing.T) {
	t.Parallel()
	b := multiBot(t, map[string][]*model.Workload{"alpha": nsWL("alpha", 1)}, "")
	listings := b.listNamespaces(multiSub, "stg", []string{"alpha"})
	now := time.Date(2026, 11, 14, 10, 20, 0, 0, time.UTC)
	m := scheduleModal(statusCluster, listings, "{}", now)
	raw, _ := json.Marshal(m)
	s := string(raw)
	for _, want := range []string{`"callback_id":"schedule_modal"`, `"type":"datetimepicker"`, fmt.Sprintf(`"initial_date_time":%d`, now.Add(time.Hour).Truncate(time.Hour).Unix()), `"block_id":"block_ns_0_0"`, `"block_id":"block_reserve_hours"`} {
		if !strings.Contains(s, want) {
			t.Errorf("modal lacks %s", want)
		}
	}
	if strings.Contains(s, "initial_options") {
		t.Error("reservation checkboxes must start unchecked")
	}
}

func TestCreateReservations_OnePerNamespaceExpandingMergedEntries(t *testing.T) {
	t.Parallel()
	ws := []*model.Workload{
		{Env: "stg", Name: "shop-api", Namespace: "alpha"}, {Env: "stg", Name: "shop-worker", Namespace: "alpha"},
		{Env: "stg", Name: "solo", Namespace: "alpha"},
	}
	b := multiBot(t, map[string][]*model.Workload{"alpha": ws, "beta": nsWL("beta", 0)}, "")
	res := &fakeReservations{}
	b.SetReservations(res)
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

	lines := b.createReservations(multiSub, statusCluster, []string{"alpha", "beta"}, scheduleInput{at: at, hours: 2 * time.Hour, names: map[string][]string{"alpha": {"shop-api"}, "beta": {"beta-w00"}}})
	if len(res.created) != 2 || !slices.Equal(res.created[0].Workloads, []string{"shop-api", "shop-worker"}) || res.created[1].Namespace != "beta" || res.created[0].Source != audit.SourceSlack {
		t.Fatalf("created = %+v", res.created)
	}
	if len(lines) != 2 || !strings.Contains(lines[0], "ready by <!date^"+fmt.Sprint(at.Unix())) || !strings.Contains(lines[0], "(starts 5m earlier), auto-stop 2h later") {
		t.Errorf("lines = %q", lines)
	}

	res.err = fmt.Errorf("%w: the time must be in the future", reservation.ErrInvalid)
	if lines := b.createReservations(multiSub, statusCluster, []string{"beta"}, scheduleInput{at: at, names: map[string][]string{"beta": {"beta-w00"}}}); len(lines) != 1 || !strings.Contains(lines[0], "⛔ beta: invalid reservation: the time must be in the future") {
		t.Errorf("error lines = %q", lines)
	}
}

func TestStatusMessage_ListsReservationsWithCancel(t *testing.T) {
	t.Parallel()
	b, _, _, out := newStatusBot(t, []*model.Workload{runningWL("web", time.Time{})}, "", "full")
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	res := &fakeReservations{list: []reservation.Reservation{{ID: "r1", Namespace: "labs", Workloads: []string{"web"}, At: at, Creator: reservation.Creator{Platform: authz.PlatformSlack, User: "UTEST0001"}}}}
	b.SetReservations(res)

	_, blocks := statusMessage(statusCluster, b.svc.Status(slackUser, "stg"), b.visibleReservations(slackUser, "stg"), nil)
	raw, _ := json.Marshal(blocks)
	s := string(raw)
	if !strings.Contains(s, "*Reservations*") || !strings.Contains(s, "by \\u003c@UTEST0001\\u003e") || !strings.Contains(s, `"action_id":"reservation_cancel"`) {
		t.Fatalf("status lacks the reservation: %s", s)
	}
	value, _ := json.Marshal(reservationRef{Env: "stg", ID: "r1"})
	b.handleReservationCancel(slack.InteractionCallback{User: slack.User{ID: "UTEST0001"}, ResponseURL: "https://hooks.example/r"}, string(value))
	if !slices.Equal(res.canceled, []string{"r1"}) || len(*out) != 1 {
		t.Fatalf("canceled %v, posted %d", res.canceled, len(*out))
	}
	posted, _ := json.Marshal((*out)[0].msg.Blocks)
	if !strings.Contains(string(posted), "Cancelled the reservation of labs / web") || strings.Contains(string(posted), "*Reservations*") {
		t.Errorf("refreshed status = %s", posted)
	}
}

func TestReservationBlocks_RespectsTheBlockBudget(t *testing.T) {
	t.Parallel()
	list := []reservation.Reservation{{ID: "r1", Namespace: "labs", Workloads: []string{"web"}}}
	if got := reservationBlocks("stg", list, 1); got != nil {
		t.Errorf("blocks with no room = %d, want none", len(got))
	}
	if got := reservationBlocks("stg", list, 5); len(got) != 2 {
		t.Errorf("blocks = %d, want a section and an actions block", len(got))
	}
}

func TestParseSlashArg_Schedule(t *testing.T) {
	t.Parallel()
	if parseSlashArg(" Schedule ") != argSchedule || parseSlashArg("schedule now") != argUnknown {
		t.Error("schedule argument not parsed")
	}
}
