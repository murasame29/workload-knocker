package mode

import "testing"

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"", Full, false},
		{"full", Full, false},
		{"scheduled-stop", ScheduledStop, false},
		{"off", Off, false},
		{"bogus", "", true},
		{"FULL", "", true},
	}
	for _, tt := range tests {
		got, err := Parse(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("Parse(%q) = %q, %v", tt.in, got, err)
		}
	}
}

func TestAllow(t *testing.T) {
	t.Parallel()
	for m, stop := range map[Mode]bool{Full: true, ScheduledStop: false, Off: false} {
		if !m.AllowAPIStart() || m.AllowAPIStop() != stop {
			t.Errorf("mode %q: unexpected permissions", m)
		}
	}
}
