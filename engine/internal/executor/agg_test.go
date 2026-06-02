package executor

import (
	"fmt"
	"testing"
)

// cell returns the single result cell at (row, col) as text.
func cell(r *Result, row, col int) string { return r.Rows[row][col].Text }

func TestAggregatesNoGroup(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY, n INT)")
	for _, n := range []int{10, 20, 30, 40} {
		run(t, ex, sess, fmt.Sprintf("INSERT INTO t (id, n) VALUES (%d, %d)", n, n))
	}

	r := run(t, ex, sess, "SELECT COUNT(*), SUM(n), AVG(n), MIN(n), MAX(n) FROM t")
	if len(r.Rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(r.Rows))
	}
	if got := cell(r, 0, 0); got != "4" {
		t.Fatalf("COUNT(*) want 4, got %s", got)
	}
	if got := cell(r, 0, 1); got != "100" {
		t.Fatalf("SUM want 100, got %s", got)
	}
	if got := cell(r, 0, 2); got != "25" {
		t.Fatalf("AVG want 25, got %s", got)
	}
	if got := cell(r, 0, 3); got != "10" {
		t.Fatalf("MIN want 10, got %s", got)
	}
	if got := cell(r, 0, 4); got != "40" {
		t.Fatalf("MAX want 40, got %s", got)
	}
}

func TestAggregateEmptyTable(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)
	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY, n INT)")

	r := run(t, ex, sess, "SELECT COUNT(*), SUM(n) FROM t")
	if len(r.Rows) != 1 {
		t.Fatalf("aggregate over empty table must yield one row, got %d", len(r.Rows))
	}
	if cell(r, 0, 0) != "0" {
		t.Fatalf("COUNT(*) over empty want 0, got %s", cell(r, 0, 0))
	}
	if !r.Rows[0][1].Null {
		t.Fatalf("SUM over empty want NULL, got %q", cell(r, 0, 1))
	}
}

func TestGroupByHaving(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE sales (id INT PRIMARY KEY, region TEXT, amount INT)")
	rows := []struct {
		id     int
		region string
		amt    int
	}{
		{1, "west", 100}, {2, "west", 200}, {3, "east", 50},
		{4, "east", 75}, {5, "east", 25}, {6, "north", 500},
	}
	for _, r := range rows {
		run(t, ex, sess, fmt.Sprintf("INSERT INTO sales (id, region, amount) VALUES (%d, '%s', %d)", r.id, r.region, r.amt))
	}

	// GROUP BY region, ordered by total amount desc.
	r := run(t, ex, sess, "SELECT region, SUM(amount), COUNT(*) FROM sales GROUP BY region ORDER BY SUM(amount) DESC")
	if len(r.Rows) != 3 {
		t.Fatalf("want 3 groups, got %d", len(r.Rows))
	}
	// north=500, west=300, east=150
	if cell(r, 0, 0) != "north" || cell(r, 0, 1) != "500" {
		t.Fatalf("first group want north/500, got %s/%s", cell(r, 0, 0), cell(r, 0, 1))
	}
	if cell(r, 1, 0) != "west" || cell(r, 1, 1) != "300" {
		t.Fatalf("second group want west/300, got %s/%s", cell(r, 1, 0), cell(r, 1, 1))
	}
	if cell(r, 2, 0) != "east" || cell(r, 2, 1) != "150" || cell(r, 2, 2) != "3" {
		t.Fatalf("third group want east/150/3, got %s/%s/%s", cell(r, 2, 0), cell(r, 2, 1), cell(r, 2, 2))
	}

	// HAVING filters groups by aggregate.
	r2 := run(t, ex, sess, "SELECT region FROM sales GROUP BY region HAVING SUM(amount) >= 300")
	if len(r2.Rows) != 2 {
		t.Fatalf("HAVING SUM>=300 want 2 groups (north, west), got %d", len(r2.Rows))
	}
}
