package periodic

import (
	"testing"
	"time"

	"github.com/murasame29/workload-knocker/internal/observability/metrics"
	"github.com/murasame29/workload-knocker/internal/policy/authz"
	"github.com/murasame29/workload-knocker/internal/schedule/autostop"
	"github.com/murasame29/workload-knocker/internal/schedule/reservation"
)

func TestNew_AutoStopRunsBeforeReservations(t *testing.T) {
	t.Parallel()
	as := autostop.New(nil, metrics.New(), time.Minute)
	rs := reservation.New(nil, authz.NewHolder(nil), reservation.Settings{})

	hooks := New(as, rs)
	if len(hooks) != 2 || hooks[0] != as || hooks[1] != rs {
		t.Errorf("hooks = %v, want auto-stop then reservations", hooks)
	}
	if got := New(nil, rs); len(got) != 1 || got[0] != rs {
		t.Errorf("without auto-stop = %v", got)
	}
}
