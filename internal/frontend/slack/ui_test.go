package slackbot

import (
	"testing"

	"github.com/murasame29/workload-knocker/internal/frontend/i18n"
)

// enUI renders in English, as the expectations of the tests are written.
var enUI = newUI(i18n.En)

func TestSlackStarter_MentionsUserIDs(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"U012AB3CD":   "<@U012AB3CD>",
		"W0123456789": "<@W0123456789>",
		"alice":       "alice",
		"api-<x>":     "api-&lt;x&gt;",
		"U12":         "U12",
		"Uabcdefghij": "Uabcdefghij",
	} {
		if got := slackStarter(in); got != want {
			t.Errorf("slackStarter(%q) = %q, want %q", in, got, want)
		}
	}
}
