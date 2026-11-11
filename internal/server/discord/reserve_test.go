package discordbot

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/murasame29/workload-knocker/internal/audit"
	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/metrics"
	"github.com/murasame29/workload-knocker/internal/mode"
	"github.com/murasame29/workload-knocker/internal/model"
	"github.com/murasame29/workload-knocker/internal/reservation"
)

var tokyo, _ = time.LoadLocation("Asia/Tokyo")

// reserveNow is 2026-11-14 19:30 in Tokyo.
var reserveNow = time.Date(2026, 11, 14, 19, 30, 0, 0, tokyo)

type fakeReservations struct {
	mu       sync.Mutex
	created  []reservation.CreateRequest
	list     []reservation.Reservation
	canceled []string
	err      error
}

func (f *fakeReservations) Create(_ context.Context, req reservation.CreateRequest) (reservation.Reservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return reservation.Reservation{}, f.err
	}
	f.created = append(f.created, req)
	return reservation.Reservation{ID: fmt.Sprintf("r%d", len(f.created)), Namespace: req.Namespace, Workloads: req.Workloads, At: req.At, Duration: req.Duration, State: reservation.Pending}, nil
}

func (f *fakeReservations) Cancel(_ context.Context, _, id string, _ authz.Subject, _ audit.Source) (reservation.Reservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canceled = append(f.canceled, id)
	for i, r := range f.list {
		if r.ID == id {
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

func reserveBot(t *testing.T, byNS map[string][]*model.Workload) (*Bot, *fakeSession, *fakeReservations) {
	t.Helper()
	cl := testCluster()
	cl.Frontends.Discord.TimeZone = "Asia/Tokyo"
	reg := &config.Config{Controller: config.Controller{Namespace: "wk"}, Clusters: []config.Cluster{cl}}
	if err := reg.Validate(); err != nil {
		t.Fatal(err)
	}
	sess := &fakeSession{}
	svc := frontend.NewService(&nsStore{byNS: byNS}, metrics.New(), time.Second,
		frontend.WithAuthorizer(authz.NewHolder(nil)), frontend.WithMode(mode.Full), frontend.WithClock(func() time.Time { return reserveNow }))
	b := newBot(sess, svc, config.NewHolder(reg), "guild-1")
	r := &fakeReservations{}
	b.SetReservations(r)
	return b, sess, r
}

func scheduleCommand(user string) *discordgo.Interaction {
	i := command(user, "knock-stg")
	i.Data = discordgo.ApplicationCommandInteractionData{Name: "knock-stg", Options: []*discordgo.ApplicationCommandInteractionDataOption{{Name: subcommandSchedule, Type: discordgo.ApplicationCommandOptionSubCommand}}}
	return i
}

// scheduleSubmit builds the raw payload of a schedule modal submission.
func scheduleSubmit(t *testing.T, user, customID string, checked map[string][]string, at, hours string) *discordgo.Event {
	t.Helper()
	comps := []map[string]any{}
	for k, v := range checked {
		comps = append(comps, map[string]any{"type": 18, "component": map[string]any{"type": 22, "custom_id": k, "values": v}})
	}
	comps = append(comps,
		map[string]any{"type": 18, "component": map[string]any{"type": 4, "custom_id": inputAt, "value": at}},
		map[string]any{"type": 18, "component": map[string]any{"type": 4, "custom_id": inputHours, "value": hours}})
	raw, err := json.Marshal(map[string]any{
		"id": "i1", "application_id": "app", "type": 5, "token": "tok",
		"member": map[string]any{"user": map[string]any{"id": user}},
		"data":   map[string]any{"custom_id": customID, "components": comps},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &discordgo.Event{Type: "INTERACTION_CREATE", RawData: raw}
}

func TestParseAt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want time.Time
		bad  bool
	}{
		{in: "21:00", want: time.Date(2026, 11, 14, 21, 0, 0, 0, tokyo)},
		{in: "19:30", want: time.Date(2026, 11, 15, 19, 30, 0, 0, tokyo)}, // now: rolls to tomorrow
		{in: " 7:05 ", want: time.Date(2026, 11, 15, 7, 5, 0, 0, tokyo)},
		{in: "2026-11-20 21:00", want: time.Date(2026, 11, 20, 21, 0, 0, 0, tokyo)},
		{in: "2026-11-20T08:15", want: time.Date(2026, 11, 20, 8, 15, 0, 0, tokyo)},
		{in: "24:00", bad: true},
		{in: "2026-02-30 10:00", bad: true},
		{in: "tonight", bad: true},
	}
	for _, tc := range cases {
		got, err := parseAt(tc.in, reserveNow, tokyo)
		if tc.bad {
			if err == nil {
				t.Errorf("%q: got %v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil || !got.Equal(tc.want) {
			t.Errorf("%q: got %v %v, want %v", tc.in, got, err, tc.want)
		}
	}
}

func TestShortDuration(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{5 * time.Minute: "5m", time.Hour: "1h", 90 * time.Minute: "1h30m", 30 * time.Second: "30s"} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestSchedule_ModalCreatesOneReservationPerNamespace(t *testing.T) {
	t.Parallel()
	b, sess, res := reserveBot(t, map[string][]*model.Workload{
		"alpha": nsWorkloads("alpha", 0, 0),
		"beta":  nsWorkloads("beta", 0),
	})
	b.dispatch(scheduleCommand("u1"))
	r := sess.responses[len(sess.responses)-1]
	if r.Type != discordgo.InteractionResponseModal {
		t.Fatalf("response = %v, want the schedule modal", r.Type)
	}
	raw, _ := json.Marshal(r.Data)
	var m struct {
		CustomID   string `json:"custom_id"`
		Components []struct {
			Type      int             `json:"type"`
			Label     string          `json:"label"`
			Component json.RawMessage `json:"component"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Components) != 4 || m.Components[0].Label != "alpha" || m.Components[2].Label != "Ready at" {
		t.Fatalf("modal labels = %+v, want alpha, beta, Ready at, Run duration", m.Components)
	}
	if c := string(m.Components[2].Component); !strings.Contains(c, `"type":4`) || strings.Contains(c, `"label"`) || !strings.Contains(c, `"required":true`) {
		t.Errorf("time input = %s, want a required text input without its own label", c)
	}

	b.handleRawEvent(scheduleSubmit(t, "u1", m.CustomID, map[string][]string{"0": {"1"}, "1": {"0"}}, "21:00", "3"))
	if len(res.created) != 2 {
		t.Fatalf("created = %+v, want one per namespace", res.created)
	}
	want := time.Date(2026, 11, 14, 21, 0, 0, 0, tokyo)
	first := res.created[0]
	if first.Namespace != "alpha" || !slices.Equal(first.Workloads, []string{"alpha-w01"}) || !first.At.Equal(want) || first.Duration != 3*time.Hour || first.Subject.User != "u1" || first.Source != audit.SourceDiscord {
		t.Errorf("first = %+v", first)
	}
	reply := sess.responses[len(sess.responses)-1]
	if reply.Data.Flags != discordgo.MessageFlagsEphemeral || !strings.Contains(reply.Data.Content, fmt.Sprintf("ready by <t:%d:f> (starts 5m earlier), auto-stop 3h later", want.Unix())) {
		t.Errorf("reply = %q", reply.Data.Content)
	}
}

func TestSchedule_RejectsBadInputAndTooManyNamespaces(t *testing.T) {
	t.Parallel()
	b, sess, res := reserveBot(t, map[string][]*model.Workload{"alpha": nsWorkloads("alpha", 0)})
	b.dispatch(scheduleCommand("u1"))
	var m struct {
		CustomID string `json:"custom_id"`
	}
	raw, _ := json.Marshal(sess.responses[len(sess.responses)-1].Data)
	_ = json.Unmarshal(raw, &m)
	b.handleRawEvent(scheduleSubmit(t, "u1", m.CustomID, map[string][]string{"0": {"0"}}, "tonight", ""))
	if got := sess.responses[len(sess.responses)-1].Data.Content; !strings.Contains(got, "⛔") || len(res.created) != 0 {
		t.Errorf("bad time reply = %q, created %v", got, res.created)
	}

	many := map[string][]*model.Workload{}
	for i := range scheduleMaxNamespaces + 1 {
		ns := fmt.Sprintf("ns-%d", i)
		many[ns] = nsWorkloads(ns, 0)
	}
	b2, sess2, _ := reserveBot(t, many)
	b2.dispatch(scheduleCommand("u1"))
	if r := sess2.responses[0]; r.Type != discordgo.InteractionResponseChannelMessageWithSource || !strings.Contains(r.Data.Content, "/api/v1/reservations") {
		t.Errorf("too many namespaces = %v %q", r.Type, r.Data.Content)
	}
}

func TestStatus_ListsReservationsWithCancel(t *testing.T) {
	t.Parallel()
	b, sess, res := reserveBot(t, map[string][]*model.Workload{"alpha": nsWorkloads("alpha", 1)})
	at := time.Date(2026, 11, 14, 21, 0, 0, 0, tokyo)
	res.list = []reservation.Reservation{{ID: "r1", Namespace: "alpha", Workloads: []string{"alpha-w00"}, At: at, State: reservation.Pending, Creator: reservation.Creator{Platform: authz.PlatformDiscord, User: "u1"}}}

	i := command("u1", "knock-stg")
	i.Data = discordgo.ApplicationCommandInteractionData{Name: "knock-stg", Options: []*discordgo.ApplicationCommandInteractionDataOption{{Name: subcommandStatus, Type: discordgo.ApplicationCommandOptionSubCommand}}}
	b.dispatch(i)
	data := sess.responses[len(sess.responses)-1].Data
	if !strings.Contains(data.Content, "**Reservations**") || !strings.Contains(data.Content, fmt.Sprintf("ready by <t:%d:f>", at.Unix())) {
		t.Fatalf("status = %q, want the reservation", data.Content)
	}
	var cancel *discordgo.Button
	for _, row := range data.Components {
		for _, c := range row.(discordgo.ActionsRow).Components {
			if btn, ok := c.(discordgo.Button); ok && strings.HasPrefix(btn.Label, "Cancel: alpha") {
				cancel = &btn
			}
		}
	}
	if cancel == nil {
		t.Fatalf("no Cancel button in %+v", data.Components)
	}
	b.dispatch(component("u1", cancel.CustomID))
	if !slices.Equal(res.canceled, []string{"r1"}) {
		t.Errorf("canceled = %v", res.canceled)
	}
	if e := sess.edits[len(sess.edits)-1]; e.Content == nil || !strings.Contains(*e.Content, "Cancelled the reservation of alpha / alpha-w00") || strings.Contains(*e.Content, "**Reservations**") {
		t.Errorf("refreshed status = %v", e.Content)
	}
}
