package executor

import (
	"fmt"
	"testing"
)

// TestExpressionIndexUsedForLookup verifies that a functional index on
// lower(email) is used to serve a `WHERE lower(email) = const` lookup.
func TestExpressionIndexUsedForLookup(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE users (id INT PRIMARY KEY, email TEXT)")
	for i := 1; i <= 20; i++ {
		run(t, ex, sess, fmt.Sprintf("INSERT INTO users (id, email) VALUES (%d, 'User%d@X.io')", i, i))
	}
	run(t, ex, sess, "CREATE INDEX idx_lower_email ON users (lower(email))")

	// Correct rows: lower('User7@X.io') = 'user7@x.io'.
	res := run(t, ex, sess, "SELECT id FROM users WHERE lower(email) = 'user7@x.io'")
	if len(res.Rows) != 1 || res.Rows[0][0].Text != "7" {
		t.Fatalf("expr lookup want id 7, got %+v", res.Rows)
	}

	// The expression-index fast path must be taken.
	rows, used, err := ex.(*execImpl).indexScanProbe(t, sess, "SELECT id FROM users WHERE lower(email) = 'user3@x.io'")
	if err != nil {
		t.Fatal(err)
	}
	if !used {
		t.Fatal("expected expression index to be used")
	}
	if len(rows) != 1 {
		t.Fatalf("expr index probe want 1 row, got %d", len(rows))
	}

	// A non-matching expression (upper) must NOT use this index.
	if _, used2, err := ex.(*execImpl).indexScanProbe(t, sess, "SELECT id FROM users WHERE upper(email) = 'X'"); err != nil {
		t.Fatal(err)
	} else if used2 {
		t.Fatal("upper(email) must not use the lower(email) index")
	}
}

// TestPartialIndexUsedWhenImplied verifies a partial index `WHERE active` is used
// for a query whose predicate implies it, and is NOT used (falls back to seq
// scan) when the query predicate does not imply the partial predicate.
func TestPartialIndexUsedWhenImplied(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE accounts (id INT PRIMARY KEY, email TEXT, active BOOL)")
	for i := 1; i <= 20; i++ {
		active := "true"
		if i%2 == 0 {
			active = "false"
		}
		run(t, ex, sess, fmt.Sprintf("INSERT INTO accounts (id, email, active) VALUES (%d, 'a%d@x.io', %s)", i, i, active))
	}
	// Partial index over active rows only.
	run(t, ex, sess, "CREATE INDEX idx_active_email ON accounts (email) WHERE active")

	// Q = `active AND email = 'a3@x.io'` implies P = `active` → index usable, and
	// id 3 is active so it is present in the partial index.
	res := run(t, ex, sess, "SELECT id FROM accounts WHERE active AND email = 'a3@x.io'")
	if len(res.Rows) != 1 || res.Rows[0][0].Text != "3" {
		t.Fatalf("partial lookup want id 3, got %+v", res.Rows)
	}
	if _, used, err := ex.(*execImpl).indexScanProbe(t, sess, "SELECT id FROM accounts WHERE active AND email = 'a5@x.io'"); err != nil {
		t.Fatal(err)
	} else if !used {
		t.Fatal("expected partial index to be used when Q implies P")
	}

	// `active = true AND email = ...` also implies `active`.
	if _, used, err := ex.(*execImpl).indexScanProbe(t, sess, "SELECT id FROM accounts WHERE active = true AND email = 'a7@x.io'"); err != nil {
		t.Fatal(err)
	} else if !used {
		t.Fatal("expected partial index to be used for active = true")
	}

	// Q = `email = 'a3@x.io'` (no active conjunct) does NOT imply P → must fall
	// back to a seq scan (not use the partial index).
	if _, used, err := ex.(*execImpl).indexScanProbe(t, sess, "SELECT id FROM accounts WHERE email = 'a3@x.io'"); err != nil {
		t.Fatal(err)
	} else if used {
		t.Fatal("partial index must NOT be used when Q does not imply P")
	}

	// And the seq-scan answer must still be correct (id 3 is active).
	res = run(t, ex, sess, "SELECT id FROM accounts WHERE email = 'a3@x.io'")
	if len(res.Rows) != 1 || res.Rows[0][0].Text != "3" {
		t.Fatalf("seq-scan fallback want id 3, got %+v", res.Rows)
	}
}
