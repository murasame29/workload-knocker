// Package nrx は newrelic-go-agent の薄い wrapper を提供する。
//
// 目的:
//   - nil-app safe な Tracer / EventRecorder インターフェースを提供する
//   - consumer から `*newrelic.Application` 直参照を排除し、テスト容易性を高める
//
// 命名注: ディレクトリ名は pkg/newrelic だが、上流 github.com/newrelic/go-agent との
// import 衝突を避けるためパッケージ名は nrx (NewRelic eXtension)。
package nrx

import (
	"github.com/newrelic/go-agent/v3/newrelic"
)

// Tracer は New Relic transaction の開始を抽象化する。
// nrApp が nil でも no-op として動作するため、consumer は nil チェック不要。
type Tracer interface {
	// Start は name でトランザクションを開始し、Span ハンドルを返す。
	// 終了時は span.End() を呼ぶ。nrApp が nil の場合は no-op span を返す。
	Start(name string) Span
	// RecordEvent は New Relic カスタムイベントを記録する。nrApp が nil の場合は no-op。
	RecordEvent(eventType string, attrs map[string]any)
}

// Span は開始済みトランザクションへの操作 (属性追加 / 終了) を提供する。
type Span interface {
	// AddAttribute は transaction にカスタム属性を追加する。
	AddAttribute(key string, value any)
	// End は transaction を終了させる。冪等。
	End()
}

// New は *newrelic.Application から Tracer を構築する。app が nil でも安全に動作する。
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

// コンパイル時 interface 充足チェック
var (
	_ Tracer = (*tracer)(nil)
	_ Span   = (*activeSpan)(nil)
	_ Span   = noopSpan{}
)
