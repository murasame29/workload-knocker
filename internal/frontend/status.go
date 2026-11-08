package frontend

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/murasame29/workload-knocker/internal/authz"
	"github.com/murasame29/workload-knocker/internal/model"
)

// StatusEntry is one entry of the status view with the data of its running members.
type StatusEntry struct {
	Entry Entry
	// StartedBy / StartedAt belong to the first running member that records them; empty / zero when unknown.
	StartedBy string
	StartedAt time.Time
	// StopAt is the earliest auto-stop among the running members; zero means none is scheduled.
	StopAt time.Time
	// ExtendNames / StopNames are the running members the subject may extend / stop now (policy and mode applied).
	ExtendNames []string
	StopNames   []string
}

// StatusNamespace is the visible entries of one namespace.
type StatusNamespace struct {
	Name    string
	Entries []StatusEntry
}

// Status is the status view of one cluster for one subject.
type Status struct {
	Env        string
	Namespaces []StatusNamespace
}

// Empty reports whether the subject sees no workload.
func (s Status) Empty() bool { return len(s.Namespaces) == 0 }

// Status lists the workloads of env that sub may operate, grouped by namespace (a-z).
// It reads the in-memory store only; no cluster call is made.
func (s *Service) Status(sub authz.Subject, env string) Status {
	out := Status{Env: env}
	namespaces := slices.Clone(s.store.Namespaces(env))
	slices.Sort(namespaces)
	for _, ns := range namespaces {
		visible := slices.DeleteFunc(slices.Clone(s.store.WorkloadsByNamespace(env, ns)), func(w *model.Workload) bool {
			return !s.operable(sub, env, ns, w.Name)
		})
		if len(visible) == 0 {
			continue
		}
		sn := StatusNamespace{Name: ns}
		for _, e := range GroupEntries(visible) {
			sn.Entries = append(sn.Entries, s.statusEntry(sub, env, ns, e))
		}
		out.Namespaces = append(out.Namespaces, sn)
	}
	return out
}

func (s *Service) statusEntry(sub authz.Subject, env, ns string, e Entry) StatusEntry {
	se := StatusEntry{Entry: e}
	for _, w := range e.Members {
		if w.Replicas == 0 {
			continue
		}
		if se.StartedBy == "" {
			se.StartedBy = w.StartedBy
		}
		if se.StartedAt.IsZero() {
			se.StartedAt = w.StartedAt
		}
		if !w.StopAt.IsZero() {
			if se.StopAt.IsZero() || w.StopAt.Before(se.StopAt) {
				se.StopAt = w.StopAt
			}
			if s.check(sub, env, ns, ActionExtend, w.Name) == "" {
				se.ExtendNames = append(se.ExtendNames, w.Name)
			}
		}
		if s.check(sub, env, ns, ActionStop, w.Name) == "" {
			se.StopNames = append(se.StopNames, w.Name)
		}
	}
	return se
}

// StatusRef is the payload of a status button: the workloads of one entry in a namespace.
type StatusRef struct {
	Env       string   `json:"e"`
	Namespace string   `json:"n"`
	Names     []string `json:"w"`
}

// EncodeStatusRef serializes ref for a button payload.
func EncodeStatusRef(ref StatusRef) string {
	b, _ := json.Marshal(ref)
	return string(b)
}

// DecodeStatusRef parses a payload built by EncodeStatusRef.
func DecodeStatusRef(payload string) (StatusRef, error) {
	var ref StatusRef
	if err := json.Unmarshal([]byte(payload), &ref); err != nil || ref.Env == "" || ref.Namespace == "" || len(ref.Names) == 0 {
		return StatusRef{}, errors.New("invalid status payload")
	}
	return ref, nil
}

// Lines renders the outcome of an extend, one line per workload; format renders the new stop time.
func (r ExtendResult) Lines(format func(time.Time) string) []string {
	var lines []string
	for _, x := range r.Extended {
		if x.Err != nil {
			lines = append(lines, fmt.Sprintf("⚠ %s: %v", x.Name, x.Err))
			continue
		}
		lines = append(lines, fmt.Sprintf("⏰ %s now stops at %s", x.Name, format(x.StopAt)))
	}
	for _, rej := range r.Rejected {
		lines = append(lines, fmt.Sprintf("⛔ %s: %s", rej.Label, rej.Message()))
	}
	return lines
}

// StopLines renders the outcome of a stop, one line per workload.
func (r ApplyResult) StopLines() []string {
	var lines []string
	for _, a := range r.Applied {
		if !a.Found {
			lines = append(lines, fmt.Sprintf("⚠ %s: not found", a.Name))
			continue
		}
		lines = append(lines, fmt.Sprintf("🔴 %s → stop", a.Name))
	}
	for _, rej := range r.Rejected {
		lines = append(lines, fmt.Sprintf("⛔ %s: %s", rej.Label, rej.Message()))
	}
	return lines
}
