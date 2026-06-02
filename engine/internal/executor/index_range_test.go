package executor

import (
	"fmt"
	"strconv"
	"testing"
)

// ids extracts the integer id column (col 0) from a result, in result order.
func ids(t *testing.T, r *Result) []int {
	t.Helper()
	out := make([]int, len(r.Rows))
	for i, row := range r.Rows {
		n, err := strconv.Atoi(row[0].Text)
		if err != nil {
			t.Fatalf("non-int id %q", row[0].Text)
		}
		out[i] = n
	}
	return out
}

// TestIndexRangeInt proves range scans return numeric (not lexicographic) order
// and correct membership over an integer secondary index.
func TestIndexRangeInt(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY, n INT)")
	for i := 1; i <= 20; i++ {
		run(t, ex, sess, fmt.Sprintf("INSERT INTO t (id, n) VALUES (%d, %d)", i, i))
	}
	run(t, ex, sess, "CREATE INDEX idx_n ON t (n)")

	// n > 9 must be {10..20} (11 rows) — lexicographic text would mishandle this.
	if got := len(run(t, ex, sess, "SELECT id FROM t WHERE n > 9").Rows); got != 11 {
		t.Fatalf("n > 9 want 11 rows, got %d", got)
	}
	if got := len(run(t, ex, sess, "SELECT id FROM t WHERE n >= 9").Rows); got != 12 {
		t.Fatalf("n >= 9 want 12, got %d", got)
	}
	if got := len(run(t, ex, sess, "SELECT id FROM t WHERE n < 5").Rows); got != 4 {
		t.Fatalf("n < 5 want 4, got %d", got)
	}
	if got := len(run(t, ex, sess, "SELECT id FROM t WHERE n <= 5").Rows); got != 5 {
		t.Fatalf("n <= 5 want 5, got %d", got)
	}
	// Bounded range via AND.
	r := run(t, ex, sess, "SELECT id FROM t WHERE n >= 5 AND n <= 8 ORDER BY n")
	if got := ids(t, r); fmt.Sprint(got) != "[5 6 7 8]" {
		t.Fatalf("range [5,8] want [5 6 7 8], got %v", got)
	}

	// Confirm the index served the range (not a full scan).
	_, used, err := ex.(*execImpl).indexScanProbe(t, sess, "SELECT id FROM t WHERE n > 9")
	if err != nil {
		t.Fatal(err)
	}
	if !used {
		t.Fatal("expected index range scan to be used")
	}
}

// TestOrderByIndexed checks ORDER BY ASC/DESC + LIMIT over an indexed column.
func TestOrderByIndexed(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY, n INT)")
	// Insert out of order.
	for _, n := range []int{7, 2, 19, 5, 11, 1, 14} {
		run(t, ex, sess, fmt.Sprintf("INSERT INTO t (id, n) VALUES (%d, %d)", n, n))
	}
	run(t, ex, sess, "CREATE INDEX idx_n ON t (n)")

	asc := ids(t, run(t, ex, sess, "SELECT n FROM t ORDER BY n"))
	if fmt.Sprint(asc) != "[1 2 5 7 11 14 19]" {
		t.Fatalf("ASC want sorted, got %v", asc)
	}
	desc := ids(t, run(t, ex, sess, "SELECT n FROM t ORDER BY n DESC"))
	if fmt.Sprint(desc) != "[19 14 11 7 5 2 1]" {
		t.Fatalf("DESC want reverse-sorted, got %v", desc)
	}
	top3 := ids(t, run(t, ex, sess, "SELECT n FROM t ORDER BY n DESC LIMIT 3"))
	if fmt.Sprint(top3) != "[19 14 11]" {
		t.Fatalf("DESC LIMIT 3 want [19 14 11], got %v", top3)
	}

	// ORDER BY served by index → probe reports index used.
	_, used, _ := ex.(*execImpl).indexScanProbe(t, sess, "SELECT n FROM t ORDER BY n DESC LIMIT 3")
	if !used {
		t.Fatal("expected ORDER BY to be served by the index")
	}
}

// TestOrderByInMemoryFallback checks ORDER BY on a NON-indexed column still
// sorts correctly (in-memory path), including numeric ordering.
func TestOrderByInMemoryFallback(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY, n INT)")
	for _, n := range []int{100, 9, 30, 2} {
		run(t, ex, sess, fmt.Sprintf("INSERT INTO t (id, n) VALUES (%d, %d)", n, n))
	}
	// No index on n.
	got := ids(t, run(t, ex, sess, "SELECT n FROM t ORDER BY n"))
	if fmt.Sprint(got) != "[2 9 30 100]" {
		t.Fatalf("in-memory numeric sort want [2 9 30 100], got %v", got)
	}
	// Probe must report NOT using the index (no index exists).
	_, used, _ := ex.(*execImpl).indexScanProbe(t, sess, "SELECT n FROM t ORDER BY n")
	if used {
		t.Fatal("did not expect index use without an index")
	}
}

// TestIndexRangeText checks lexicographic range + ORDER BY on a text index.
func TestIndexRangeText(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY, s TEXT)")
	for i, s := range []string{"apple", "banana", "cherry", "date", "elder"} {
		run(t, ex, sess, fmt.Sprintf("INSERT INTO t (id, s) VALUES (%d, '%s')", i, s))
	}
	run(t, ex, sess, "CREATE INDEX idx_s ON t (s)")

	r := run(t, ex, sess, "SELECT s FROM t WHERE s >= 'banana' AND s < 'date' ORDER BY s")
	var got []string
	for _, row := range r.Rows {
		got = append(got, row[0].Text)
	}
	if fmt.Sprint(got) != "[banana cherry]" {
		t.Fatalf("text range want [banana cherry], got %v", got)
	}
}
