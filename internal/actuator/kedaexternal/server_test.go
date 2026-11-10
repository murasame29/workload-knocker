package kedaexternal

import (
	"context"
	"testing"
	"time"
)

func TestNewServer_Validation(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		port      int
		cert, key string
		wantOK    bool
	}{
		"plaintext":             {port: 9090, wantOK: true},
		"port zero":             {port: 0},
		"port too large":        {port: 70000},
		"cert without key":      {port: 9090, cert: "c.pem"},
		"key without cert":      {port: 9090, key: "k.pem"},
		"cert and key together": {port: 9090, cert: "c.pem", key: "k.pem", wantOK: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := NewServer(NewHub(), tc.port, tc.cert, tc.key)
			if (err == nil) != tc.wantOK {
				t.Errorf("NewServer error = %v, want ok=%v", err, tc.wantOK)
			}
		})
	}
}

func TestServer_ShutdownStopsStart(t *testing.T) {
	t.Parallel()

	s, err := NewServer(NewHub(), 38091, "", "")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Start(context.Background()) }()
	time.Sleep(100 * time.Millisecond)

	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start = %v, want nil", err)
		}
	case <-time.After(wait):
		t.Fatal("Start did not return after Shutdown")
	}
}
