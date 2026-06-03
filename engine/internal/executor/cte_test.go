package executor

import (
	"strings"
	"testing"
)

// TestCTE covers non-recursive WITH: a single CTE used in FROM, a CTE filtered
// in the outer query, multiple CTEs where a later one references an earlier one,
// and that "timestamp with time zone" still parses (WITH is now a keyword).
func TestCTE(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE emp (id INT PRIMARY KEY, name TEXT, dept TEXT, salary INT)")
	run(t, ex, sess, "INSERT INTO emp (id, name, dept, salary) VALUES (1, 'a', 'eng', 100)")
	run(t, ex, sess, "INSERT INTO emp (id, name, dept, salary) VALUES (2, 'b', 'eng', 200)")
	run(t, ex, sess, "INSERT INTO emp (id, name, dept, salary) VALUES (3, 'c', 'sales', 150)")

	// Single CTE used directly.
	r := run(t, ex, sess, "WITH eng AS (SELECT name, salary FROM emp WHERE dept = 'eng') SELECT name FROM eng ORDER BY salary")
	if len(r.Rows) != 2 || r.Rows[0][0].Text != "a" || r.Rows[1][0].Text != "b" {
		t.Fatalf("single CTE: want [a b], got %v", col0(r))
	}

	// Outer query filters the CTE.
	r = run(t, ex, sess, "WITH eng AS (SELECT name, salary FROM emp WHERE dept = 'eng') SELECT name FROM eng WHERE salary > 150")
	if len(r.Rows) != 1 || r.Rows[0][0].Text != "b" {
		t.Fatalf("CTE + outer WHERE: want [b], got %v", col0(r))
	}

	// Chained CTEs: the second references the first.
	r = run(t, ex, sess, `WITH a AS (SELECT name, salary FROM emp WHERE salary >= 150),
		b AS (SELECT name FROM a WHERE salary < 200)
		SELECT name FROM b`)
	if len(r.Rows) != 1 || r.Rows[0][0].Text != "c" {
		t.Fatalf("chained CTE: want [c], got %v", col0(r))
	}
}

// TestTimestampWithTimeZoneStillParses guards the WITH-keyword change: the
// multi-word type name must still be accepted in DDL.
func TestTimestampWithTimeZoneStillParses(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE evt (id INT PRIMARY KEY, at TIMESTAMP WITH TIME ZONE)")
	r := run(t, ex, sess, "SELECT data_type FROM information_schema.columns WHERE table_name = 'evt' AND column_name = 'at'")
	if len(r.Rows) == 0 || !strings.Contains(r.Rows[0][0].Text, "with time zone") {
		t.Fatalf("timestamptz column data_type wrong: %v", col0(r))
	}
}
