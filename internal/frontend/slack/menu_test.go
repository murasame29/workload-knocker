package slackbot

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/observability/metrics"
	"github.com/murasame29/workload-knocker/internal/policy/mode"
	"github.com/murasame29/workload-knocker/internal/workload/model"
)

// fakeViews records opened and replaced modals.
type fakeViews struct {
	mu      sync.Mutex
	opened  []slack.ModalViewRequest
	updated []slack.ModalViewRequest
	viewIDs []string
}

func (f *fakeViews) OpenView(_ string, v slack.ModalViewRequest) (*slack.ViewResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, v)
	return &slack.ViewResponse{}, nil
}

func (f *fakeViews) UpdateView(v slack.ModalViewRequest, _, _, viewID string) (*slack.ViewResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updated = append(f.updated, v)
	f.viewIDs = append(f.viewIDs, viewID)
	return &slack.ViewResponse{}, nil
}

func (f *fakeViews) last(t *testing.T) slack.ModalViewRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.updated) == 0 {
		t.Fatal("no modal was updated")
	}
	return f.updated[len(f.updated)-1]
}

func (s *multiStore) Reconcile(context.Context, string) error { return nil }

// menuBot is multiBot with the registry and a recording view API.
func menuBot(t *testing.T, byNS map[string][]*model.Workload) (*Bot, *fakeViews) {
	t.Helper()
	b := multiBot(t, byNS, "")
	holder := config.NewHolder(helperConfig(t, statusCluster))
	b.svc = frontend.NewService(&multiStore{byNS: byNS}, metrics.New(), time.Second,
		frontend.WithAuthorizer(b.authz), frontend.WithMode(mode.Full), frontend.WithClusters(holder))
	b.registry = holder
	views := &fakeViews{}
	b.views = views
	return b, views
}

func viewCallback(t *testing.T, callbackID string, meta any) slack.InteractionCallback {
	t.Helper()
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	return slack.InteractionCallback{
		User: slack.User{ID: multiSub.User},
		View: slack.View{ID: "V1", Hash: "h1", CallbackID: callbackID, PrivateMetadata: string(raw)},
	}
}

func modalJSON(t *testing.T, v slack.ModalViewRequest) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestSlashCommand_NoArgumentOpensTheMenu(t *testing.T) {
	t.Parallel()
	b, views := menuBot(t, map[string][]*model.Workload{"alpha": nsWL("alpha", 0)})

	b.handleSlashCommand(slack.SlashCommand{Command: "/knock-stg", TriggerID: "T1", ChannelID: "C1", UserID: multiSub.User})

	if len(views.opened) != 1 || views.opened[0].CallbackID != callbackMenuModal {
		t.Fatalf("opened = %+v, want the menu", views.opened)
	}
	s := modalJSON(t, views.opened[0])
	for _, id := range []string{actionMenuStartStop, actionMenuSchedule, actionMenuStatus} {
		if !strings.Contains(s, `"action_id":"`+id+`"`) {
			t.Errorf("menu lacks %s: %s", id, s)
		}
	}
	if views.opened[0].Submit != nil {
		t.Error("the menu has no submit button")
	}
}

func TestMenu_EachButtonReplacesTheModalWithItsFlow(t *testing.T) {
	t.Parallel()
	byNS := map[string][]*model.Workload{"alpha": nsWL("alpha", 1, 0), "beta": nsWL("beta", 0)}
	meta := modalMetadata{ChannelID: "C1", UserID: multiSub.User, Cluster: "stg"}

	cases := []struct {
		action       string
		reservations bool
		callback     string
		contains     string
	}{
		{actionMenuStartStop, false, callbackWorkloadModal, `"block_id":"block_ns_1_0"`},
		{actionMenuSchedule, true, callbackScheduleModal, `"action_id":"schedule_namespace"`},
		{actionMenuSchedule, false, "", "Reservations are not available"},
		{actionMenuStatus, false, callbackStatusModal, `"action_id":"status_namespace"`},
	}
	for _, tc := range cases {
		b, views := menuBot(t, byNS)
		if tc.reservations {
			b.SetReservations(&fakeReservations{})
		}
		b.handleMenu(viewCallback(t, callbackMenuModal, meta), tc.action)
		got := views.last(t)
		if got.CallbackID != tc.callback || !strings.Contains(modalJSON(t, got), tc.contains) || views.viewIDs[0] != "V1" {
			t.Errorf("%s (reservations %v) = %s %s, want %s with %s", tc.action, tc.reservations, got.CallbackID, modalJSON(t, got), tc.callback, tc.contains)
		}
	}
}

func TestMenu_NoVisibleNamespaceShowsANotice(t *testing.T) {
	t.Parallel()
	b, views := menuBot(t, map[string][]*model.Workload{})
	b.handleMenu(viewCallback(t, callbackMenuModal, modalMetadata{Cluster: "stg"}), actionMenuStatus)
	if got := views.last(t); got.CallbackID != "" || !strings.Contains(blockTexts(got.Blocks.BlockSet), "No managed workloads") {
		t.Errorf("notice = %s", modalJSON(t, got))
	}
}

func TestScheduleNamespace_FormOfThatNamespace(t *testing.T) {
	t.Parallel()
	b, views := menuBot(t, map[string][]*model.Workload{"alpha": nsWL("alpha", 0), "beta": nsWL("beta", 0, 0)})
	b.SetReservations(&fakeReservations{})

	b.handleScheduleNamespace(viewCallback(t, callbackScheduleModal, scheduleMetadata{ChannelID: "C1", UserID: multiSub.User, Cluster: "stg"}), "beta")

	got := views.last(t)
	s := modalJSON(t, got)
	if !strings.Contains(s, "beta-w01") || strings.Contains(s, "alpha-w00") || got.Submit == nil {
		t.Errorf("schedule form = %s", s)
	}
	var meta scheduleMetadata
	if err := json.Unmarshal([]byte(got.PrivateMetadata), &meta); err != nil || len(meta.Namespaces) != 1 || meta.Namespaces[0] != "beta" || meta.ChannelID != "C1" {
		t.Errorf("metadata = %+v %v", meta, err)
	}
}

func TestStatusNamespace_ListsOnlyThatNamespace(t *testing.T) {
	t.Parallel()
	b, views := menuBot(t, map[string][]*model.Workload{"alpha": nsWL("alpha", 1), "beta": nsWL("beta", 1)})

	b.handleStatusNamespace(viewCallback(t, callbackStatusModal, statusMetadata{Cluster: "stg"}), "beta")

	got := views.last(t)
	text := blockTexts(got.Blocks.BlockSet)
	if !strings.Contains(text, "beta-w00") || strings.Contains(text, "alpha-w00") || got.CallbackID != callbackStatusModal {
		t.Errorf("status = %s", text)
	}
	var meta statusMetadata
	if err := json.Unmarshal([]byte(got.PrivateMetadata), &meta); err != nil || meta.Namespace != "beta" {
		t.Errorf("metadata = %+v %v", meta, err)
	}
}

func TestStatusAction_InTheModalRefreshesTheModal(t *testing.T) {
	t.Parallel()
	b, store, _, out := newStatusBot(t, []*model.Workload{runningWL("web", time.Now().Add(time.Hour))}, "", "full")
	views := &fakeViews{}
	b.views = views
	value := frontend.EncodeStatusRef(frontend.StatusRef{Env: "stg", Namespace: "labs", Names: []string{"web"}})
	cb := viewCallback(t, callbackStatusModal, statusMetadata{Cluster: "stg", Namespace: "labs"})
	cb.User = slack.User{ID: slackUser.User, Name: "alice"}

	b.handleStatusAction(cb, actionStatusStop, value)

	if len(store.stops) != 1 || len(*out) != 0 {
		t.Fatalf("stops %v, webhook posts %d", store.stops, len(*out))
	}
	got := views.last(t)
	if got.CallbackID != callbackStatusModal || !strings.Contains(blockTexts(got.Blocks.BlockSet), "web") || views.viewIDs[0] != "V1" {
		t.Errorf("refreshed modal = %s", modalJSON(t, got))
	}
}
