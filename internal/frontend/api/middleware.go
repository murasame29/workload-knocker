package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/murasame29/workload-knocker/internal/observability/audit"
	"github.com/murasame29/workload-knocker/internal/policy/mode"
)

// Authenticate requires "Authorization: Bearer <token>" and fails closed (503) when token is empty.
func Authenticate(token string, rec audit.Recorder, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			writeJSONError(w, http.StatusServiceUnavailable, "API is disabled because API_TOKEN is not set")
			return
		}
		scheme, got, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			unauthorized(w)
			recordDenied(rec, r)
			return
		}
		sum := sha256.Sum256([]byte(strings.TrimSpace(got)))
		if subtle.ConstantTimeCompare(sum[:], want[:]) != 1 {
			unauthorized(w)
			recordDenied(rec, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireStop rejects the request with 403 unless the mode allows API stops.
func RequireStop(m mode.Mode, rec audit.Recorder, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.AllowAPIStop() {
			writeJSONError(w, http.StatusForbidden,
				"stop is not allowed in OPERATION_MODE="+string(m)+"; use the chat command or the scheduled stop")
			recordDenied(rec, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeJSONError(w, http.StatusUnauthorized, "missing or invalid bearer token")
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	b, _ := json.Marshal(map[string]string{"message": msg})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

// SourceHeader lets the nightly CronJob identify itself; any other value is recorded as "api".
const SourceHeader = "X-Workload-Knocker-Source"

// sourceOf returns the audit source of a stop-all request.
func sourceOf(r *http.Request) audit.Source {
	if r.Header.Get(SourceHeader) == string(audit.SourceNightlyStop) {
		return audit.SourceNightlyStop
	}
	return audit.SourceAPI
}

// recordDenied emits a workload-less denied event for a request refused before reaching a handler.
// Read-only GET requests are not audited: the events describe workload operations.
func recordDenied(rec audit.Recorder, r *http.Request) {
	if r.Method == http.MethodGet {
		return
	}
	event, source := audit.WorkloadStart, audit.SourceAPI
	if strings.HasSuffix(r.URL.Path, "/stop-all") {
		event, source = audit.WorkloadStopAll, sourceOf(r)
	}
	rec.Record(r.Context(), audit.Record{
		Event: event, Outcome: audit.Denied, Source: source,
		RequestID: audit.NewRequestID(), EndUser: audit.EndUserAPI,
	})
}
