package slackbot

import (
	"strings"
	"testing"

	"github.com/slack-go/slack"

	"github.com/murasame29/workload-knocker/internal/frontend"
	"github.com/murasame29/workload-knocker/internal/frontend/i18n"
	"github.com/murasame29/workload-knocker/internal/workload/model"
)

func TestViewer_Started(t *testing.T) {
	t.Parallel()
	v := viewer{ID: "U012AB3CD", Name: "alice"}
	for starter, want := range map[string]bool{"alice": true, "U012AB3CD": true, "": true, "bob": false, "U999ZZ999": false} {
		if got := v.started(starter); got != want {
			t.Errorf("started(%q) = %v, want %v", starter, got, want)
		}
	}
	if (viewer{ID: "U1"}).started("") != true || (viewer{ID: "U1"}).started("x") {
		t.Error("a viewer without a name matches the ID only")
	}
}

func statusEntry(starter string) frontend.StatusEntry {
	w := &model.Workload{Env: "stg", Namespace: "labs", Name: "web", Replicas: 1, StartedBy: starter}
	return frontend.StatusEntry{Entry: frontend.Entry{Key: "web", Members: []*model.Workload{w}}, StartedBy: starter, ExtendNames: []string{"web"}, StopNames: []string{"web"}}
}

func buttonsOf(t *testing.T, blk slack.Block) map[string]*slack.ButtonBlockElement {
	t.Helper()
	out := map[string]*slack.ButtonBlockElement{}
	for _, e := range blk.(*slack.ActionBlock).Elements.ElementSet {
		btn := e.(*slack.ButtonBlockElement)
		out[btn.ActionID] = btn
	}
	return out
}

func TestEntryButtons_ConfirmStopOfSomeoneElsesWorkload(t *testing.T) {
	t.Parallel()
	alice := viewer{ID: "UALICE111", Name: "alice"}

	own := buttonsOf(t, enUI.entryButtons("stg", "labs", statusEntry("alice"), alice))
	if own[actionStatusStop].Confirm != nil {
		t.Error("stopping your own workload needs no confirmation")
	}
	reserved := buttonsOf(t, enUI.entryButtons("stg", "labs", statusEntry("UALICE111"), alice))
	if reserved[actionStatusStop].Confirm != nil {
		t.Error("a reservation of your own (recorded by user ID) needs no confirmation")
	}

	other := buttonsOf(t, enUI.entryButtons("stg", "labs", statusEntry("UBOB22222"), alice))
	c := other[actionStatusStop].Confirm
	if c == nil {
		t.Fatal("stopping someone else's workload must ask for confirmation")
	}
	if !strings.Contains(c.Text.Text, "*web*") || !strings.Contains(c.Text.Text, "<@UBOB22222>") || c.Text.Type != "mrkdwn" || c.Confirm.Text != "Stop" || c.Deny.Text != "Cancel" || c.Style != slack.StyleDanger {
		t.Errorf("confirm = %+v %+v", c.Text, c)
	}
	if other[actionStatusExtend].Confirm != nil {
		t.Error("Extend never asks for confirmation")
	}

	ja := buttonsOf(t, newUI(i18n.Ja).entryButtons("stg", "labs", statusEntry("bob"), alice))[actionStatusStop].Confirm
	if ja == nil || !strings.Contains(ja.Text.Text, "あなたは、このworkloadを起動した本人ではないです。") || !strings.Contains(ja.Text.Text, "起動者: bob") || ja.Confirm.Text != "停止" || ja.Deny.Text != "キャンセル" {
		t.Errorf("japanese confirm = %+v", ja)
	}
}

func TestForeignStopLines_WarnsInThePreview(t *testing.T) {
	t.Parallel()
	entries := []frontend.Entry{
		{Key: "web", Members: []*model.Workload{{Name: "web", Replicas: 1, StartedBy: "bob"}}},
		{Key: "mine", Members: []*model.Workload{{Name: "mine", Replicas: 1, StartedBy: "alice"}}},
		{Key: "new", Members: []*model.Workload{{Name: "new"}}},
	}
	changes := []frontend.Change{
		{Action: frontend.ActionStop, Label: "web", Names: []string{"web"}},
		{Action: frontend.ActionStop, Label: "mine", Names: []string{"mine"}},
		{Action: frontend.ActionStart, Label: "new", Names: []string{"new"}},
	}
	lines := enUI.foreignStopLines(changes, entries, viewer{ID: "UALICE111", Name: "alice"})
	if len(lines) != 2 || !strings.Contains(lines[0], "Stop: *web*  Started by: bob") || !strings.Contains(lines[1], "You did not start") {
		t.Errorf("lines = %q", lines)
	}
	if got := enUI.foreignStopLines(changes[1:], entries, viewer{Name: "alice"}); got != nil {
		t.Errorf("own stops warned: %q", got)
	}
}
