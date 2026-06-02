package executor

import (
	"fmt"
	"testing"
)

// TestCompositeIndexLookup verifies a multi-column index serves an equality
// lookup on all its columns, and maintenance across INSERT/UPDATE/DELETE.
func TestCompositeIndexLookup(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE events (id INT PRIMARY KEY, region TEXT, kind TEXT)")
	regions := []string{"west", "east"}
	kinds := []string{"click", "view"}
	id := 0
	for _, r := range regions {
		for _, k := range kinds {
			for i := 0; i < 5; i++ {
				id++
				run(t, ex, sess, fmt.Sprintf("INSERT INTO events (id, region, kind) VALUES (%d, '%s', '%s')", id, r, k))
			}
		}
	}
	run(t, ex, sess, "CREATE INDEX idx_rk ON events (region, kind)")

	// Equality on both columns → 5 rows.
	r := run(t, ex, sess, "SELECT id FROM events WHERE region = 'west' AND kind = 'view'")
	if len(r.Rows) != 5 {
		t.Fatalf("composite lookup want 5 rows, got %d", len(r.Rows))
	}
	// Confirm the composite index path was used.
	_, used, err := ex.(*execImpl).indexScanProbe(t, sess, "SELECT id FROM events WHERE region = 'east' AND kind = 'click'")
	if err != nil {
		t.Fatal(err)
	}
	if !used {
		t.Fatal("expected composite index to be used")
	}

	// Buckets: west/click ids 1-5, west/view ids 6-10, east/click 11-15, east/view 16-20.
	// Maintenance: UPDATE moves a west/view row (id 6) into west/click.
	run(t, ex, sess, "UPDATE events SET kind = 'click' WHERE id = 6")
	if got := len(run(t, ex, sess, "SELECT id FROM events WHERE region = 'west' AND kind = 'view'").Rows); got != 4 {
		t.Fatalf("after update want 4 west/view, got %d", got)
	}
	if got := len(run(t, ex, sess, "SELECT id FROM events WHERE region = 'west' AND kind = 'click'").Rows); got != 6 {
		t.Fatalf("after update want 6 west/click, got %d", got)
	}
	// DELETE removes a west/view row (id 7).
	run(t, ex, sess, "DELETE FROM events WHERE id = 7")
	if got := len(run(t, ex, sess, "SELECT id FROM events WHERE region = 'west' AND kind = 'view'").Rows); got != 3 {
		t.Fatalf("after delete want 3 west/view, got %d", got)
	}
}

// TestCompositeUnique verifies a UNIQUE composite index enforces tuple
// uniqueness (the same value in one column is fine if the other differs).
func TestCompositeUnique(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE m (id INT PRIMARY KEY, a TEXT, b TEXT)")
	run(t, ex, sess, "INSERT INTO m (id, a, b) VALUES (1, 'x', 'p')")
	run(t, ex, sess, "INSERT INTO m (id, a, b) VALUES (2, 'x', 'q')") // same a, different b — OK before index
	run(t, ex, sess, "CREATE UNIQUE INDEX uq_ab ON m (a, b)")

	// Same (a,b) tuple as id=1 → violation.
	if err := execErr(ex, sess, "INSERT INTO m (id, a, b) VALUES (3, 'x', 'p')"); err == nil {
		t.Fatal("expected composite unique violation for (x,p)")
	}
	// Same a but different b → allowed.
	run(t, ex, sess, "INSERT INTO m (id, a, b) VALUES (4, 'x', 'r')")
}
