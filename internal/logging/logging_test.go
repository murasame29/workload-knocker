package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestNew_RenamesKeysAndStampsService(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	New(&buf, slog.LevelInfo, "9.9.9-test").Info("hello", slog.String("k", "v"))

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"time", "msg"} {
		if _, ok := got[k]; ok {
			t.Errorf("%q should be renamed: %v", k, got)
		}
	}
	want := map[string]any{"message": "hello", "level": "INFO", "k": "v", "service.name": "workload-knocker", "service.version": "9.9.9-test"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if _, ok := got["timestamp"]; !ok {
		t.Errorf("missing timestamp: %v", got)
	}
}

func TestReplaceAttr_LeavesGroupedKeys(t *testing.T) {
	t.Parallel()
	if got := ReplaceAttr([]string{"g"}, slog.String("msg", "x")); got.Key != "msg" {
		t.Errorf("grouped key renamed: %v", got)
	}
}
