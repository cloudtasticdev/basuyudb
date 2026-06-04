package executor

import (
	"testing"
	"time"
)

// TestCompositePrimaryKey (BUG 1) — a table-level composite PRIMARY KEY over
// (a, b) must be accepted (not rejected with 42P16 "multiple primary keys") and
// enforced as a unique constraint on the pair. A single-column table-level PK
// still works, and a genuine double PRIMARY KEY declaration still errors 42P16.
func TestCompositePrimaryKey(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	// Composite table-level PK is accepted.
	run(t, ex, sess, "CREATE TABLE p2 (a INT, b INT, PRIMARY KEY (a, b))")

	// It is enforced over the pair: (1,2) twice violates; (1,3) is fine.
	run(t, ex, sess, "INSERT INTO p2 (a, b) VALUES (1, 2)")
	run(t, ex, sess, "INSERT INTO p2 (a, b) VALUES (1, 3)")
	if err := execErr(ex, sess, "INSERT INTO p2 (a, b) VALUES (1, 2)"); err == nil {
		t.Fatal("expected composite PK violation on duplicate (1,2)")
	}

	// Single-column table-level PK still works.
	run(t, ex, sess, "CREATE TABLE p1 (a INT, b INT, PRIMARY KEY (a))")
	run(t, ex, sess, "INSERT INTO p1 (a, b) VALUES (1, 1)")
	if err := execErr(ex, sess, "INSERT INTO p1 (a, b) VALUES (1, 2)"); err == nil {
		t.Fatal("expected single-column PK violation on duplicate a=1")
	}

	// A genuine double PRIMARY KEY (inline PK + table-level PK) still errors 42P16.
	if err := execErr(ex, sess, "CREATE TABLE p3 (id INT PRIMARY KEY, a INT, PRIMARY KEY (a))"); err == nil {
		t.Fatal("expected 42P16 for two primary keys")
	}
}

// TestRecursiveCTERegression (BUG 2) — both the explicit column-list form
// `cnt(n)` and the AS-alias form must produce {1,2,3,4,5}, count 5, and must
// terminate promptly (the working-table fix prevents non-termination).
func TestRecursiveCTERegression(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)

		// Column-list form: WITH RECURSIVE cnt(n) AS (...).
		r := run(t, ex, sess,
			"WITH RECURSIVE cnt(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM cnt WHERE n < 5) "+
				"SELECT array_agg(n) FROM cnt")
		if len(r.Rows) != 1 {
			t.Errorf("column-list recursive CTE: want 1 row, got %d", len(r.Rows))
			return
		}
		if got := r.Rows[0][0].Text; got != "{1,2,3,4,5}" {
			t.Errorf("column-list recursive CTE array_agg: want {1,2,3,4,5}, got %q", got)
		}

		// AS-alias form: WITH RECURSIVE cnt AS (SELECT 1 AS n ...).
		r2 := run(t, ex, sess,
			"WITH RECURSIVE cnt AS (SELECT 1 AS n UNION ALL SELECT n+1 FROM cnt WHERE n < 5) "+
				"SELECT count(*) FROM cnt")
		if len(r2.Rows) != 1 || r2.Rows[0][0].Text != "5" {
			t.Errorf("AS-alias recursive CTE count(*): want 5, got %v", r2.Rows)
		}
	}()

	select {
	case <-doneCh:
	case <-time.After(15 * time.Second):
		t.Fatal("recursive CTE did not terminate (possible non-termination regression)")
	}
}

// TestArrayLiteralCastExec (BUG 3) — `'literal'::TYPE[]` casts execute and yield
// the array type. The overlap operator over two literal-cast int arrays is true.
func TestArrayLiteralCastExec(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	r := run(t, ex, sess, "SELECT '{1,2,3}'::int[] && '{3,4}'::int[]")
	if len(r.Rows) != 1 || r.Rows[0][0].Text != "t" {
		t.Fatalf("int[] overlap: want true, got %v", r.Rows)
	}

	r = run(t, ex, sess, "SELECT '{a,b}'::text[]")
	if len(r.Rows) != 1 || r.Rows[0][0].Text != "{a,b}" {
		t.Fatalf("text[] cast: want {a,b}, got %v", r.Rows)
	}

	r = run(t, ex, sess, "SELECT ARRAY[1,2]::int[]")
	if len(r.Rows) != 1 || r.Rows[0][0].Text != "{1,2}" {
		t.Fatalf("ARRAY cast: want {1,2}, got %v", r.Rows)
	}
}

// TestUnnestBareAliasColumn (BUG 4) — a single set-returning function in FROM
// with a bare alias `AS u` names both the relation and its single output column,
// so `u` is visible to ORDER BY and WHERE.
func TestUnnestBareAliasColumn(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	r := run(t, ex, sess, "SELECT u FROM unnest(ARRAY[30,10,20]) AS u ORDER BY u")
	if r.Columns[0].Name != "u" {
		t.Fatalf("output column name: want u, got %q", r.Columns[0].Name)
	}
	if len(r.Rows) != 3 ||
		r.Rows[0][0].Text != "10" || r.Rows[1][0].Text != "20" || r.Rows[2][0].Text != "30" {
		t.Fatalf("unnest ORDER BY u: want 10,20,30, got %v", col0(r))
	}

	r = run(t, ex, sess, "SELECT u FROM unnest(ARRAY[30,10,20]) AS u WHERE u > 10")
	if len(r.Rows) != 2 {
		t.Fatalf("unnest WHERE u > 10: want 2 rows, got %v", col0(r))
	}
}

// TestCommaLateralBinding (BUG 5) — a comma-separated (implicit cross join)
// LATERAL subquery binds its alias and evaluates correlated against the
// preceding FROM items, like explicit JOIN LATERAL. The latent comma cross-join
// bug (only the first FROM item materialized) is covered too.
func TestCommaLateralBinding(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	// Plain comma cross join materializes BOTH items.
	r := run(t, ex, sess, "SELECT p.id, l.mx FROM (SELECT 1 id) p, (SELECT 2 mx) l")
	if len(r.Rows) != 1 || r.Rows[0][0].Text != "1" || r.Rows[0][1].Text != "2" {
		t.Fatalf("comma cross join: want (1,2), got %v", r.Rows)
	}

	// Comma LATERAL with a constant subquery.
	r = run(t, ex, sess, "SELECT p.id, l.mx FROM (SELECT 1 id) p, LATERAL (SELECT 2 mx) l")
	if len(r.Rows) != 1 || r.Rows[0][0].Text != "1" || r.Rows[0][1].Text != "2" {
		t.Fatalf("comma LATERAL: want (1,2), got %v", r.Rows)
	}

	// Correlated comma LATERAL: the subquery references a preceding FROM item.
	run(t, ex, sess, "CREATE TABLE t (id INT)")
	run(t, ex, sess, "CREATE TABLE s (k INT, x INT)")
	run(t, ex, sess, "INSERT INTO t (id) VALUES (1), (2)")
	run(t, ex, sess, "INSERT INTO s (k, x) VALUES (1, 10), (1, 20), (2, 5)")
	r = run(t, ex, sess,
		"SELECT a.id, l.mx FROM t a, LATERAL (SELECT max(x) mx FROM s WHERE s.k = a.id) l ORDER BY a.id")
	if len(r.Rows) != 2 ||
		r.Rows[0][0].Text != "1" || r.Rows[0][1].Text != "20" ||
		r.Rows[1][0].Text != "2" || r.Rows[1][1].Text != "5" {
		t.Fatalf("correlated comma LATERAL: want (1,20),(2,5), got %v", r.Rows)
	}
}
