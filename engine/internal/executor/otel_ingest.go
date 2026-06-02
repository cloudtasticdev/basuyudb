package executor

import (
	"context"

	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// Span is a single OpenTelemetry span in the shape the otel_spans table stores.
// The OTLP receiver (engine/internal/otel) converts protobuf spans into this
// struct and calls IngestSpans; the same rows are then queryable via SQL and
// joinable with relational tables (Gate 3).
type Span struct {
	TraceID string
	SpanID string
	ParentSpanID string
	ServiceName string
	SpanName string
	DurationMS int64
	Status string
	StartedAt string
	AttributesJSON string // JSON object, queryable with ->>
}

// SpanIngester is the narrow surface the OTLP receiver depends on.
type SpanIngester interface {
	IngestSpans(ctx context.Context, sess *session.Session, spans []Span) error
}

// IngestSpans writes spans to the otel_spans table in one transaction, keyed by
// (trace_id, span_id) via OtelSpanKey. Cell order matches otelSpansSchema.
func (e *execImpl) IngestSpans(ctx context.Context, sess *session.Session, spans []Span) error {
	if len(spans) == 0 {
		return nil
	}
	txn, err := e.txn.Begin(ctx, sess.Auth)
	if err != nil {
		return err
	}
	defer e.txn.Rollback(ctx, txn)

	enc := e.store.Encoder()
	for _, s := range spans {
		cells := []Datum{
			{Text: s.TraceID},
			{Text: s.SpanID},
			{Text: s.ParentSpanID, Null: s.ParentSpanID == ""},
			{Text: s.ServiceName},
			{Text: s.SpanName},
			{Text: itoa(s.DurationMS)},
			{Text: s.Status},
			{Text: s.StartedAt},
			{Text: s.AttributesJSON, Null: s.AttributesJSON == ""},
		}
		key := enc.OtelSpanKey(sess.Namespace(), sess.Branch(), []byte(s.TraceID), []byte(s.SpanID))
		e.txn.Buffer(txn, transactions.Mutation{Key: key, Value: encodeRow(cells)})
	}
	return e.txn.Commit(ctx, txn)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
