package executor

import (
	"testing"
)

// TestMultiColumnForeignKey verifies composite FK enforcement (the FULL tuple
// must match a parent) and ON DELETE CASCADE across the composite key. It also
// exercises table-level PRIMARY KEY / FOREIGN KEY declared in CREATE TABLE.
func TestMultiColumnForeignKey(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	// Parent with a composite primary key (table-level PRIMARY KEY).
	run(t, ex, sess, "CREATE TABLE parent (a INT, b INT, label TEXT, PRIMARY KEY (a, b))")
	run(t, ex, sess, "INSERT INTO parent (a, b, label) VALUES (1, 10, 'p1')")
	run(t, ex, sess, "INSERT INTO parent (a, b, label) VALUES (1, 20, 'p2')")
	run(t, ex, sess, "INSERT INTO parent (a, b, label) VALUES (2, 10, 'p3')")

	// Child with a composite FK (table-level FOREIGN KEY ... ON DELETE CASCADE).
	run(t, ex, sess, "CREATE TABLE child (id INT PRIMARY KEY, pa INT, pb INT, "+
		"FOREIGN KEY (pa, pb) REFERENCES parent (a, b) ON DELETE CASCADE)")

	// (1,10) and (2,10) exist → these inserts succeed.
	run(t, ex, sess, "INSERT INTO child (id, pa, pb) VALUES (100, 1, 10)")
	run(t, ex, sess, "INSERT INTO child (id, pa, pb) VALUES (101, 2, 10)")

	// (1,99) is NOT a parent tuple even though a=1 and b=99 each appear in some
	// row individually — the FULL tuple must match. This must be rejected.
	if err := execErr(ex, sess, "INSERT INTO child (id, pa, pb) VALUES (102, 1, 99)"); err == nil {
		t.Fatal("expected composite FK violation for (1,99)")
	}
	// (2,20) likewise is not a parent tuple (only (2,10) exists).
	if err := execErr(ex, sess, "INSERT INTO child (id, pa, pb) VALUES (103, 2, 20)"); err == nil {
		t.Fatal("expected composite FK violation for (2,20)")
	}

	// ON DELETE CASCADE on the full key: deleting parent (1,10) removes child 100
	// but leaves child 101 (which references (2,10)).
	run(t, ex, sess, "DELETE FROM parent WHERE a = 1 AND b = 10")
	if got := len(run(t, ex, sess, "SELECT id FROM child WHERE id = 100").Rows); got != 0 {
		t.Fatalf("cascade should have deleted child 100, still present")
	}
	if got := len(run(t, ex, sess, "SELECT id FROM child WHERE id = 101").Rows); got != 1 {
		t.Fatalf("child 101 should survive cascade, got %d", got)
	}
}

// TestTableLevelConstraintsInCreate verifies table-level PRIMARY KEY, UNIQUE, and
// CHECK declared in the CREATE TABLE body are enforced.
func TestTableLevelConstraintsInCreate(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE t ("+
		"id INT, region TEXT, code TEXT, qty INT, "+
		"PRIMARY KEY (id), "+
		"UNIQUE (region, code), "+
		"CHECK (qty >= 0))")

	run(t, ex, sess, "INSERT INTO t (id, region, code, qty) VALUES (1, 'w', 'x', 5)")

	// PRIMARY KEY (id) implies NOT NULL + uniqueness: duplicate id rejected.
	if err := execErr(ex, sess, "INSERT INTO t (id, region, code, qty) VALUES (1, 'e', 'y', 1)"); err == nil {
		t.Fatal("expected PK violation on duplicate id")
	}
	// UNIQUE (region, code): duplicate tuple rejected.
	if err := execErr(ex, sess, "INSERT INTO t (id, region, code, qty) VALUES (2, 'w', 'x', 1)"); err == nil {
		t.Fatal("expected UNIQUE violation on (w,x)")
	}
	// Same region, different code → allowed.
	run(t, ex, sess, "INSERT INTO t (id, region, code, qty) VALUES (3, 'w', 'z', 1)")
	// CHECK (qty >= 0): negative rejected.
	if err := execErr(ex, sess, "INSERT INTO t (id, region, code, qty) VALUES (4, 'e', 'q', -1)"); err == nil {
		t.Fatal("expected CHECK violation on qty = -1")
	}
}
