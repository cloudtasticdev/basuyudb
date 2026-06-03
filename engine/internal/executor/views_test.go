package executor

import (
	"strings"
	"testing"
)

// TestViews covers CREATE VIEW, querying it (with an outer WHERE), filtering and
// joining views, CREATE OR REPLACE, and DROP VIEW.
func TestViews(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE emp (id INT PRIMARY KEY, name TEXT, dept TEXT, salary INT)")
	run(t, ex, sess, "INSERT INTO emp (id, name, dept, salary) VALUES (1, 'a', 'eng', 100)")
	run(t, ex, sess, "INSERT INTO emp (id, name, dept, salary) VALUES (2, 'b', 'eng', 200)")
	run(t, ex, sess, "INSERT INTO emp (id, name, dept, salary) VALUES (3, 'c', 'sales', 150)")

	// Create and query a view.
	run(t, ex, sess, "CREATE VIEW eng AS SELECT id, name, salary FROM emp WHERE dept = 'eng'")
	r := run(t, ex, sess, "SELECT name FROM eng ORDER BY salary")
	if len(r.Rows) != 2 || r.Rows[0][0].Text != "a" || r.Rows[1][0].Text != "b" {
		t.Fatalf("view query: want [a b], got %v", col0(r))
	}

	// Outer query filters the view.
	r = run(t, ex, sess, "SELECT name FROM eng WHERE salary > 150")
	if len(r.Rows) != 1 || r.Rows[0][0].Text != "b" {
		t.Fatalf("view + outer WHERE: want [b], got %v", col0(r))
	}

	// A view over an aggregate.
	run(t, ex, sess, "CREATE VIEW dept_avg AS SELECT dept, count(*) AS n FROM emp GROUP BY dept")
	r = run(t, ex, sess, "SELECT dept, n FROM dept_avg ORDER BY dept")
	if len(r.Rows) != 2 {
		t.Fatalf("aggregate view: want 2 rows, got %d", len(r.Rows))
	}

	// CREATE OR REPLACE redefines the view.
	run(t, ex, sess, "CREATE OR REPLACE VIEW eng AS SELECT id, name FROM emp WHERE dept = 'sales'")
	r = run(t, ex, sess, "SELECT name FROM eng")
	if len(r.Rows) != 1 || r.Rows[0][0].Text != "c" {
		t.Fatalf("replaced view: want [c], got %v", col0(r))
	}

	// A view does not appear as a base table in information_schema.tables.
	r = run(t, ex, sess, "SELECT table_name FROM information_schema.tables WHERE table_name = 'eng'")
	if len(r.Rows) != 0 {
		t.Fatalf("view should not appear in information_schema.tables, got %v", col0(r))
	}

	// DROP VIEW removes it.
	run(t, ex, sess, "DROP VIEW eng")
	if err := execErr(ex, sess, "SELECT * FROM eng"); err == nil {
		t.Fatalf("expected error querying dropped view")
	}

	// Creating a view that shadows a real table is rejected.
	if err := execErr(ex, sess, "CREATE VIEW emp AS SELECT id FROM emp"); err == nil {
		t.Fatalf("expected error creating view that shadows a table")
	} else if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected already-exists error, got %v", err)
	}
}
