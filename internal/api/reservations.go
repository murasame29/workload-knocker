package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/murasame29/workload-knocker/internal/audit"
	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/reservation"
	"github.com/murasame29/workload-knocker/internal/runlimit"
)

// reservationsPath is the collection; a reservation is reservationsPath + "/<id>".
const reservationsPath = "/api/v1/reservations"

// Reservations is the part of reservation.Service the handler needs.
type Reservations interface {
	Create(ctx context.Context, req reservation.CreateRequest) (reservation.Reservation, error)
	List(ctx context.Context, env string) ([]reservation.Reservation, error)
	Cancel(ctx context.Context, env, id string, sub authz.Subject, source audit.Source) (reservation.Reservation, error)
}

// ReservationsHandler handles /api/v1/reservations: POST creates, GET lists, DELETE cancels.
// Every call acts as the api subject of the authorization policy.
type ReservationsHandler struct {
	svc Reservations
}

// NewReservationsHandler constructs a ReservationsHandler.
func NewReservationsHandler(svc Reservations) *ReservationsHandler {
	return &ReservationsHandler{svc: svc}
}

type reservationRequest struct {
	Env       string   `json:"env"`
	Namespace string   `json:"namespace"`
	Workloads []string `json:"workloads"`
	// At is when the workloads should be Ready (RFC 3339). Required.
	At string `json:"at"`
	// Duration is the run length after At as a Go duration ("3h"); empty uses the default.
	Duration string `json:"duration"`
}

type reservationJSON struct {
	ID        string    `json:"id"`
	Env       string    `json:"env"`
	Namespace string    `json:"namespace"`
	Workloads []string  `json:"workloads"`
	At        time.Time `json:"at"`
	Duration  string    `json:"duration,omitempty"`
	State     string    `json:"state"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	Note      string    `json:"note,omitempty"`
}

func toJSON(env string, r reservation.Reservation) reservationJSON {
	out := reservationJSON{
		ID: r.ID, Env: env, Namespace: r.Namespace, Workloads: r.Workloads, At: r.At, State: string(r.State),
		CreatedBy: audit.EndUser(audit.Source(r.Creator.Platform), r.Creator.User), CreatedAt: r.CreatedAt, Note: r.Note,
	}
	if r.Duration > 0 {
		out.Duration = r.Duration.String()
	}
	return out
}

// ServeHTTP implements net/http.Handler.
//
// Status codes:
//   - POST /api/v1/reservations: 201 created, 400 invalid input, 403 refused by the policy, 404 unknown env or workload
//   - GET /api/v1/reservations?env=<env>: 200 with the reservations (finished ones are kept for a day), 404 unknown env
//   - DELETE /api/v1/reservations/<id>?env=<env>: 200 cancelled, 403 refused by the policy, 404 no such pending reservation
func (h *ReservationsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, reservationsPath), "/")
	switch {
	case id == "" && r.Method == http.MethodPost:
		h.create(w, r)
	case id == "" && r.Method == http.MethodGet:
		h.list(w, r)
	case id != "" && !strings.Contains(id, "/") && r.Method == http.MethodDelete:
		h.cancel(w, r, id)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (h *ReservationsHandler) create(w http.ResponseWriter, r *http.Request) {
	var req reservationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if req.Env == "" || req.Namespace == "" || req.At == "" {
		http.Error(w, "env, namespace and at are required", http.StatusBadRequest)
		return
	}
	at, err := time.Parse(time.RFC3339, req.At)
	if err != nil {
		http.Error(w, "invalid at: use RFC 3339 such as 2026-11-14T21:00:00+09:00", http.StatusBadRequest)
		return
	}
	d, err := runlimit.ParseGo(req.Duration)
	if err != nil {
		http.Error(w, "invalid duration: "+err.Error(), http.StatusBadRequest)
		return
	}
	res, err := h.svc.Create(r.Context(), reservation.CreateRequest{
		Env: req.Env, Namespace: req.Namespace, Workloads: req.Workloads, At: at, Duration: d,
		Subject: authz.APISubject, Source: audit.SourceAPI,
	})
	if err != nil {
		reservationError(w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusCreated, toJSON(req.Env, res))
}

func (h *ReservationsHandler) list(w http.ResponseWriter, r *http.Request) {
	env := r.URL.Query().Get("env")
	if env == "" {
		http.Error(w, "env is required", http.StatusBadRequest)
		return
	}
	list, err := h.svc.List(r.Context(), env)
	if err != nil {
		reservationError(w, err)
		return
	}
	out := make([]reservationJSON, 0, len(list))
	for _, res := range list {
		out = append(out, toJSON(env, res))
	}
	writeJSON(r.Context(), w, http.StatusOK, map[string]any{"env": env, "reservations": out})
}

func (h *ReservationsHandler) cancel(w http.ResponseWriter, r *http.Request, id string) {
	env := r.URL.Query().Get("env")
	if env == "" {
		http.Error(w, "env is required", http.StatusBadRequest)
		return
	}
	res, err := h.svc.Cancel(r.Context(), env, id, authz.APISubject, audit.SourceAPI)
	if err != nil {
		reservationError(w, err)
		return
	}
	writeJSON(r.Context(), w, http.StatusOK, toJSON(env, res))
}

// reservationError maps a reservation error to its status; other errors are a 500 without details.
func reservationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, reservation.ErrInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, reservation.ErrForbidden):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, reservation.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	default:
		slog.Error("reservation request failed", slog.String("error.message", err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func writeJSON(ctx context.Context, w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.ErrorContext(ctx, "failed to encode response", slog.String("error.type", fmt.Sprintf("%T", err)), slog.String("error.message", err.Error()))
	}
}
