package version

import (
	"runtime/debug"
	"testing"
)

func TestResolve(t *testing.T) {
	bi := &debug.BuildInfo{
		Main: debug.Module{Version: "v1.2.3"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abc123"},
			{Key: "vcs.time", Value: "2026-01-02T03:04:05Z"},
		},
	}
	devel := &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}

	tests := []struct {
		name    string
		v, c, d string
		bi      *debug.BuildInfo
		ok      bool
		want    string
	}{
		{"ldflags set wins", "1.0.0", "deadbeef", "2026-05-01T00:00:00Z", bi, true,
			"workload-knocker version=1.0.0 commit=deadbeef build_date=2026-05-01T00:00:00Z"},
		{"fallback to build info", "dev", "unknown", "unknown", bi, true,
			"workload-knocker version=v1.2.3 commit=abc123 build_date=2026-01-02T03:04:05Z"},
		{"devel module version ignored", "dev", "unknown", "unknown", devel, true,
			"workload-knocker version=dev commit=unknown build_date=unknown"},
		{"no build info", "dev", "unknown", "unknown", nil, false,
			"workload-knocker version=dev commit=unknown build_date=unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolve(tt.v, tt.c, tt.d, tt.bi, tt.ok).String(); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
