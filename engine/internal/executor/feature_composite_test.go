package executor

import (
	"testing"
)

// TestCompositeTypeRowCastFieldAccess verifies CREATE TYPE ... AS (...) and
// field access on a (ROW(...)::typename) value.
func TestCompositeTypeRowCastFieldAccess(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TYPE addr AS (street TEXT, zip INT)")

	res := run(t, ex, sess, "SELECT (ROW('main st', 90210)::addr).zip")
	if len(res.Rows) != 1 || res.Rows[0][0].Text != "90210" {
		t.Fatalf("(ROW(...)::addr).zip want 90210, got %+v", res.Rows)
	}
	res = run(t, ex, sess, "SELECT (ROW('main st', 90210)::addr).street")
	if len(res.Rows) != 1 || res.Rows[0][0].Text != "main st" {
		t.Fatalf("(ROW(...)::addr).street want 'main st', got %+v", res.Rows)
	}

	// Unknown field is a clear error, not a crash.
	if err := execErr(ex, sess, "SELECT (ROW('main st', 90210)::addr).nope"); err == nil {
		t.Fatal("expected error for unknown composite field")
	}
}

// TestCompositeTypedColumnFieldAccess verifies field access on a composite-typed
// column: (col).field decodes the stored record value.
func TestCompositeTypedColumnFieldAccess(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TYPE addr AS (street TEXT, zip INT)")
	run(t, ex, sess, "CREATE TABLE people (id INT PRIMARY KEY, home addr)")
	run(t, ex, sess, "INSERT INTO people (id, home) VALUES (1, ROW('elm st', 12345)::addr)")

	res := run(t, ex, sess, "SELECT (home).zip FROM people WHERE id = 1")
	if len(res.Rows) != 1 || res.Rows[0][0].Text != "12345" {
		t.Fatalf("(home).zip want 12345, got %+v", res.Rows)
	}
	res = run(t, ex, sess, "SELECT (home).street FROM people WHERE id = 1")
	if len(res.Rows) != 1 || res.Rows[0][0].Text != "elm st" {
		t.Fatalf("(home).street want 'elm st', got %+v", res.Rows)
	}
}
