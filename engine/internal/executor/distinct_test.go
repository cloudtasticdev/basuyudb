package executor

import "testing"

// TestSelectDistinct proves SELECT DISTINCT dedups result rows, including with
// ORDER BY and LIMIT, and across multiple projected columns.
func TestSelectDistinct(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY, city TEXT, kind TEXT)")
	run(t, ex, sess, "INSERT INTO t (id, city, kind) VALUES (1, 'west', 'view')")
	run(t, ex, sess, "INSERT INTO t (id, city, kind) VALUES (2, 'west', 'view')")
	run(t, ex, sess, "INSERT INTO t (id, city, kind) VALUES (3, 'east', 'click')")
	run(t, ex, sess, "INSERT INTO t (id, city, kind) VALUES (4, 'west', 'click')")

	// Single-column DISTINCT.
	r := run(t, ex, sess, "SELECT DISTINCT city FROM t ORDER BY city")
	if len(r.Rows) != 2 || r.Rows[0][0].Text != "east" || r.Rows[1][0].Text != "west" {
		t.Fatalf("DISTINCT city: want [east west], got %v", rowsText(r))
	}

	// Multi-column DISTINCT: (west,view) collapses, others distinct -> 3 rows.
	r = run(t, ex, sess, "SELECT DISTINCT city, kind FROM t")
	if len(r.Rows) != 3 {
		t.Fatalf("DISTINCT city,kind: want 3 rows, got %d (%v)", len(r.Rows), rowsText(r))
	}

	// DISTINCT + LIMIT applies the limit AFTER dedup.
	r = run(t, ex, sess, "SELECT DISTINCT city FROM t ORDER BY city LIMIT 1")
	if len(r.Rows) != 1 || r.Rows[0][0].Text != "east" {
		t.Fatalf("DISTINCT + LIMIT: want [east], got %v", rowsText(r))
	}

	// Non-DISTINCT still returns all rows (no accidental dedup).
	r = run(t, ex, sess, "SELECT city FROM t")
	if len(r.Rows) != 4 {
		t.Fatalf("plain SELECT: want 4 rows, got %d", len(r.Rows))
	}
}

func rowsText(r *Result) []string {
	out := make([]string, len(r.Rows))
	for i, row := range r.Rows {
		s := ""
		for _, d := range row {
			if d.Null {
				s += "NULL,"
			} else {
				s += d.Text + ","
			}
		}
		out[i] = s
	}
	return out
}
