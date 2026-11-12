package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/murasame29/workload-knocker/internal/observability/audit"
	"github.com/murasame29/workload-knocker/internal/policy/authz"
	"github.com/murasame29/workload-knocker/internal/schedule/reservation"
)

type fakeReservations struct {
	created []reservation.CreateRequest
	err     error
	list    []reservation.Reservation
	cancel  string
}

func (f *fakeReservations) Create(_ context.Context, req reservation.CreateRequest) (reservation.Reservation, error) {
	if f.err != nil {
		return reservation.Reservation{}, f.err
	}
	f.created = append(f.created, req)
	return reservation.Reservation{ID: "r1", Namespace: req.Namespace, Workloads: req.Workloads, At: req.At, Duration: req.Duration, State: reservation.Pending, Creator: reservation.CreatorOf(req.Subject)}, nil
}

func (f *fakeReservations) List(context.Context, string) ([]reservation.Reservation, error) {
	return f.list, f.err
}

func (f *fakeReservations) Cancel(_ context.Context, _, id string, sub authz.Subject, src audit.Source) (reservation.Reservation, error) {
	if f.err != nil {
		return reservation.Reservation{}, f.err
	}
	if sub.Platform != authz.PlatformAPI || src != audit.SourceAPI {
		return reservation.Reservation{}, fmt.Errorf("unexpected subject %+v / %s", sub, src)
	}
	f.cancel = id
	return reservation.Reservation{ID: id, State: reservation.Cancelled}, nil
}

func call(h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, strings.NewReader(body)))
	return rec
}

func TestReservations_CreateListCancel(t *testing.T) {
	t.Parallel()
	f := &fakeReservations{}
	h := NewReservationsHandler(f)

	rec := call(h, http.MethodPost, "/api/v1/reservations", `{"env":"stg","namespace":"games","workloads":["terraria"],"at":"2026-11-14T21:00:00+09:00","duration":"3h"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	got := f.created[0]
	if !got.At.Equal(time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)) || got.Duration != 3*time.Hour || got.Subject.Platform != authz.PlatformAPI || got.Source != audit.SourceAPI {
		t.Errorf("request = %+v", got)
	}
	var body reservationJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.ID != "r1" || body.State != "pending" || body.Duration != "3h0m0s" || body.CreatedBy != "api" {
		t.Errorf("body = %+v %v", body, err)
	}

	f.list = []reservation.Reservation{{ID: "r1", State: reservation.Pending}}
	if rec := call(h, http.MethodGet, "/api/v1/reservations?env=stg", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"r1"`) {
		t.Errorf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := call(h, http.MethodDelete, "/api/v1/reservations/r1?env=stg", ""); rec.Code != http.StatusOK || f.cancel != "r1" {
		t.Errorf("cancel: %d %s", rec.Code, rec.Body)
	}
}

func TestReservations_Errors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, method, target, body string
		err                        error
		want                       int
	}{
		{"bad time", http.MethodPost, "/api/v1/reservations", `{"env":"stg","namespace":"games","workloads":["a"],"at":"21:00"}`, nil, http.StatusBadRequest},
		{"bad duration", http.MethodPost, "/api/v1/reservations", `{"env":"stg","namespace":"games","workloads":["a"],"at":"2026-11-14T21:00:00Z","duration":"x"}`, nil, http.StatusBadRequest},
		{"invalid", http.MethodPost, "/api/v1/reservations", `{"env":"stg","namespace":"games","workloads":["a"],"at":"2026-11-14T21:00:00Z"}`, reservation.ErrInvalid, http.StatusBadRequest},
		{"forbidden", http.MethodPost, "/api/v1/reservations", `{"env":"stg","namespace":"games","workloads":["a"],"at":"2026-11-14T21:00:00Z"}`, reservation.ErrForbidden, http.StatusForbidden},
		{"not found", http.MethodDelete, "/api/v1/reservations/nope?env=stg", "", reservation.ErrNotFound, http.StatusNotFound},
		{"internal", http.MethodGet, "/api/v1/reservations?env=stg", "", fmt.Errorf("api down"), http.StatusInternalServerError},
		{"missing env", http.MethodGet, "/api/v1/reservations", "", nil, http.StatusBadRequest},
		{"method", http.MethodPut, "/api/v1/reservations", "", nil, http.StatusMethodNotAllowed},
		{"nested path", http.MethodDelete, "/api/v1/reservations/a/b?env=stg", "", nil, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		h := NewReservationsHandler(&fakeReservations{err: tc.err})
		if rec := call(h, tc.method, tc.target, tc.body); rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body)
		}
	}
}
