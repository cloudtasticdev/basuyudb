package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudtasticdev/basuyudb/engine/internal/parser"
)

// TestAnonRecordPositionalField covers `.fN` positional access on an anonymous
// record produced by ROW(...) with no declared composite type.
func TestAnonRecordPositionalField(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	res := run(t, ex, sess, "SELECT (ROW(10, 'x', 30)).f1, (ROW(10, 'x', 30)).f2, (ROW(10, 'x', 30)).f3")
	if len(res.Rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(res.Rows))
	}
	r := res.Rows[0]
	if r[0].Text != "10" || r[1].Text != "x" || r[2].Text != "30" {
		t.Fatalf("anon positional access mismatch: %+v", r)
	}
}

// TestNestedAnonRecordField covers nested `((x).a).b` where the intermediate is
// itself an anonymous record.
func TestNestedAnonRecordField(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	res := run(t, ex, sess, "SELECT ((ROW(ROW(10, 20), 3)).f1).f2")
	if len(res.Rows) != 1 || res.Rows[0][0].Text != "20" {
		t.Fatalf("nested anon access want 20, got %+v", res.Rows)
	}

	// First-level field that is itself a record can be selected as text.
	res = run(t, ex, sess, "SELECT ((ROW(ROW(10, 20), 3)).f1).f1")
	if len(res.Rows) != 1 || res.Rows[0][0].Text != "10" {
		t.Fatalf("nested anon access f1.f1 want 10, got %+v", res.Rows)
	}
}

// TestAnonRecordFieldErrors verifies that out-of-range and non-positional field
// names yield a clear error rather than crashing.
func TestAnonRecordFieldErrors(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	mustErr := func(sql, wantState string) {
		stmt, perr := parser.Parse(sql)
		if perr != nil {
			t.Fatalf("parse %q: %v", sql, perr)
		}
		_, err := ex.Execute(context.Background(), stmt, sess, nil)
		if err == nil {
			t.Fatalf("%q: expected error, got nil", sql)
		}
		var ee *ExecError
		if !errors.As(err, &ee) || ee.SQLSTATE != wantState {
			t.Fatalf("%q: want SQLSTATE %s, got %v", sql, wantState, err)
		}
	}

	// f9 out of range on a 3-field record.
	mustErr("SELECT (ROW(1,2,3)).f9", "42703")
	// Non-positional field name on an anonymous record.
	mustErr("SELECT (ROW(1,2,3)).bogus", "42703")
}
