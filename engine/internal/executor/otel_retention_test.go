package executor

import (
	"context"
	"testing"
)

func TestOTelRetentionSweep(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)
	ei := ex.(*execImpl)
	bg := context.Background()

	spans := []Span{
		{TraceID: "t1", SpanID: "s1", ServiceName: "svc", SpanName: "old1", StartedAt: "2020-01-01T00:00:00Z"},
		{TraceID: "t2", SpanID: "s2", ServiceName: "svc", SpanName: "old2", StartedAt: "2021-06-01T00:00:00Z"},
		{TraceID: "t3", SpanID: "s3", ServiceName: "svc", SpanName: "new1", StartedAt: "2026-01-01T00:00:00Z"},
	}
	if err := ei.IngestSpans(bg, sess, spans); err != nil {
		t.Fatal(err)
	}
	if got := cell(run(t, ex, sess, "SELECT COUNT(*) FROM otel_spans"), 0, 0); got != "3" {
		t.Fatalf("want 3 spans before sweep, got %s", got)
	}

	// Delete spans started before 2023.
	n, err := ex.SweepOTelRetention(bg, sess, "2023-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("retention sweep want 2 removed, got %d", n)
	}
	if got := cell(run(t, ex, sess, "SELECT COUNT(*) FROM otel_spans"), 0, 0); got != "1" {
		t.Fatalf("want 1 span after sweep, got %s", got)
	}
	if got := cell(run(t, ex, sess, "SELECT span_name FROM otel_spans"), 0, 0); got != "new1" {
		t.Fatalf("surviving span should be new1, got %s", got)
	}
}
