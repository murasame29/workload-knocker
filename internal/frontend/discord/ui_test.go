package discordbot

import (
	"testing"

	"github.com/murasame29/workload-knocker/internal/frontend/i18n"
)

// enUI renders in English, as the expectations of the tests are written.
var enUI = newUI(i18n.En)

func TestMessageConstants_AreTranslated(t *testing.T) {
	t.Parallel()
	ja := newUI(i18n.Ja)
	for _, m := range []string{msgExpired, msgForeign, msgClusterGone, msgUnrecognized, msgNotAuthorized} {
		if ja.t(m) == m {
			t.Errorf("no Japanese for %q", m)
		}
	}
}
