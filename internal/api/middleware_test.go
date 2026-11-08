package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/murasame29/workload-knocker/internal/mode"
)

const testToken = "test-token-value"

func chain(token string, m mode.Mode, stop bool) http.Handler {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	if stop {
		return Authenticate(token, RequireStop(m, ok))
	}
	return Authenticate(token, ok)
}

func do(h http.Handler, authz string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/x", strings.NewReader("{}"))
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAuthenticate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		token string
		authz string
		want  int
	}{
		{"missing", testToken, "", http.StatusUnauthorized},
		{"wrong scheme", testToken, "Basic " + testToken, http.StatusUnauthorized},
		{"wrong token", testToken, "Bearer nope", http.StatusUnauthorized},
		{"longer wrong token", testToken, "Bearer " + testToken + "x", http.StatusUnauthorized},
		{"right token", testToken, "Bearer " + testToken, http.StatusOK},
		{"fail closed", "", "Bearer anything", http.StatusServiceUnavailable},
		{"fail closed empty bearer", "", "Bearer ", http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := do(chain(tt.token, mode.Full, false), tt.authz)
			if rec.Code != tt.want {
				t.Fatalf("got %d, want %d", rec.Code, tt.want)
			}
			if tt.want == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("missing WWW-Authenticate header")
			}
		})
	}
}

func TestModeVsEndpoint(t *testing.T) {
	t.Parallel()
	good := "Bearer " + testToken
	tests := []struct {
		m     mode.Mode
		stop  bool
		authz string
		want  int
	}{
		{mode.Full, true, good, http.StatusOK},
		{mode.Full, false, good, http.StatusOK},
		{mode.ScheduledStop, true, good, http.StatusForbidden},
		{mode.ScheduledStop, false, good, http.StatusOK},
		{mode.Off, true, good, http.StatusForbidden},
		{mode.Off, false, good, http.StatusOK},
		{mode.Off, true, "", http.StatusUnauthorized}, // auth before mode
	}
	for _, tt := range tests {
		rec := do(chain(testToken, tt.m, tt.stop), tt.authz)
		if rec.Code != tt.want {
			t.Errorf("mode=%s stop=%v authz=%q: got %d, want %d", tt.m, tt.stop, tt.authz, rec.Code, tt.want)
		}
	}
}
