package config

import (
	"testing"
	"time"
)

func TestEnv_ValidateRunDuration(t *testing.T) {
	t.Parallel()

	ok := Env{DefaultRunDuration: 0, MaxRunDuration: 24 * time.Hour, StopReminder: 15 * time.Minute}
	if err := ok.ValidateRunDuration(); err != nil {
		t.Errorf("defaults: %v", err)
	}
	if err := (&Env{DefaultRunDuration: 4 * time.Hour, MaxRunDuration: 4 * time.Hour}).ValidateRunDuration(); err != nil {
		t.Errorf("default equal to max: %v", err)
	}
	bad := map[string]Env{
		"max below minimum":    {MaxRunDuration: time.Minute},
		"default below min":    {DefaultRunDuration: time.Minute, MaxRunDuration: time.Hour},
		"negative default":     {DefaultRunDuration: -time.Hour, MaxRunDuration: time.Hour},
		"default above max":    {DefaultRunDuration: 5 * time.Hour, MaxRunDuration: 4 * time.Hour},
		"negative stop remind": {MaxRunDuration: time.Hour, StopReminder: -time.Minute},
	}
	for name, e := range bad {
		if err := e.ValidateRunDuration(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestEnv_RunDefaults(t *testing.T) {
	t.Parallel()

	e := Env{DefaultRunDuration: time.Hour, MaxRunDuration: 2 * time.Hour}
	if d := e.RunDefaults(); d.Default != time.Hour || d.Max != 2*time.Hour {
		t.Errorf("defaults = %+v", d)
	}
}
