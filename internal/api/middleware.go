package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/murasame29/workload-knocker/internal/mode"
)

// Authenticate requires "Authorization: Bearer <token>" and fails closed (503) when token is empty.
func Authenticate(token string, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			writeJSONError(w, http.StatusServiceUnavailable, "API is disabled because API_TOKEN is not set")
			return
		}
		scheme, got, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			unauthorized(w)
			return
		}
		sum := sha256.Sum256([]byte(strings.TrimSpace(got)))
		if subtle.ConstantTimeCompare(sum[:], want[:]) != 1 {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireStop rejects the request with 403 unless the mode allows API stops.
func RequireStop(m mode.Mode, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.AllowAPIStop() {
			writeJSONError(w, http.StatusForbidden,
				"stop is not allowed in OPERATION_MODE="+string(m)+"; use the Slack command or the scheduled stop")
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
