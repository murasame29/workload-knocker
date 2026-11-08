// Package nrx is a thin wrapper around newrelic-go-agent with nil-app safe Tracer interfaces.
//
// The package is named nrx to avoid an import clash with upstream github.com/newrelic/go-agent.
package nrx

import (
	"github.com/newrelic/go-agent/v3/newrelic"
)

// Tracer abstracts starting New Relic transactions.
// It is a no-op when nrApp is nil, so consumers need no nil check.
type Tracer interface {
	// Start begins a transaction named name and returns a Span; call span.End() when done.
	Start(name string) Span
	// RecordEvent records a New Relic custom event.
	RecordEvent(eventType string, attrs map[string]any)
}

// Span operates on a started transaction.
type Span interface {
	// AddAttribute adds a custom attribute to the transaction.
	AddAttribute(key string, value any)
	// End finishes the transaction. Idempotent.
	End()
}

// New builds a Tracer from *newrelic.Application. A nil app is safe.
func New(app *newrelic.Application) Tracer {
	return &tracer{app: app}
}

type tracer struct {
	app *newrelic.Application
}

func (t *tracer) Start(name string) Span {
	if t.app == nil {
		return noopSpan{}
	}
	return &activeSpan{txn: t.app.StartTransaction(name)}
}

func (t *tracer) RecordEvent(eventType string, attrs map[string]any) {
	if t.app == nil {
		return
	}
	t.app.RecordCustomEvent(eventType, attrs)
}

type activeSpan struct {
	txn *newrelic.Transaction
}

func (s *activeSpan) AddAttribute(key string, value any) {
	s.txn.AddAttribute(key, value)
}

func (s *activeSpan) End() {
	s.txn.End()
}

type noopSpan struct{}

func (noopSpan) AddAttribute(string, any) {}
func (noopSpan) End()                     {}

// Compile-time interface check.
var (
	_ Tracer = (*tracer)(nil)
	_ Span   = (*activeSpan)(nil)
	_ Span   = noopSpan{}
)
