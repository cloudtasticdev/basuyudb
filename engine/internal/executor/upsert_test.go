package executor

import (
	"testing"
)

func TestOnConflictDoNothing(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)
	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY, n INT)")
	run(t, ex, sess, "INSERT INTO t (id, n) VALUES (1, 10)")

	// Conflicting insert is silently ignored.
	r := run(t, ex, sess, "INSERT INTO t (id, n) VALUES (1, 999) ON CONFLICT (id) DO NOTHING")
	if r.RowsAffected != 0 {
		t.Fatalf("DO NOTHING want 0 rows affected, got %d", r.RowsAffected)
	}
	// Original value preserved.
	if got := cell(run(t, ex, sess, "SELECT n FROM t WHERE id = 1"), 0, 0); got != "10" {
		t.Fatalf("DO NOTHING must keep original, got %s", got)
	}
}

func TestOnConflictDoUpdate(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)
	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY, n INT, hits INT)")
	run(t, ex, sess, "INSERT INTO t (id, n, hits) VALUES (1, 10, 1)")

	// Upsert: on conflict, set n to the proposed value (EXCLUDED) and bump hits.
	run(t, ex, sess, "INSERT INTO t (id, n, hits) VALUES (1, 20, 0) ON CONFLICT (id) DO UPDATE SET n = EXCLUDED.n, hits = hits + 1")
	r := run(t, ex, sess, "SELECT n, hits FROM t WHERE id = 1")
	if r.Rows[0][0].Text != "20" {
		t.Fatalf("upsert: n should be EXCLUDED value 20, got %s", r.Rows[0][0].Text)
	}
	if r.Rows[0][1].Text != "2" {
		t.Fatalf("upsert: hits should be existing+1=2, got %s", r.Rows[0][1].Text)
	}
	// Still one row (updated, not inserted).
	if got := cell(run(t, ex, sess, "SELECT COUNT(*) FROM t"), 0, 0); got != "1" {
		t.Fatalf("upsert must not add a row, got count %s", got)
	}

	// A non-conflicting insert with the same clause still inserts.
	run(t, ex, sess, "INSERT INTO t (id, n, hits) VALUES (2, 5, 0) ON CONFLICT (id) DO UPDATE SET n = EXCLUDED.n")
	if got := cell(run(t, ex, sess, "SELECT COUNT(*) FROM t"), 0, 0); got != "2" {
		t.Fatalf("non-conflicting upsert should insert, count %s", got)
	}
}
