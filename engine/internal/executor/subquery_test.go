package executor

import (
	"fmt"
	"testing"
)

func TestInValueList(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY, n INT)")
	for i := 1; i <= 6; i++ {
		run(t, ex, sess, fmt.Sprintf("INSERT INTO t (id, n) VALUES (%d, %d)", i, i*10))
	}
	r := run(t, ex, sess, "SELECT id FROM t WHERE n IN (20, 40, 60)")
	if len(r.Rows) != 3 {
		t.Fatalf("IN list want 3 rows, got %d", len(r.Rows))
	}
	rn := run(t, ex, sess, "SELECT id FROM t WHERE n NOT IN (20, 40, 60)")
	if len(rn.Rows) != 3 {
		t.Fatalf("NOT IN list want 3 rows, got %d", len(rn.Rows))
	}
}

func TestInSubquery(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE orders (id INT PRIMARY KEY, customer INT)")
	run(t, ex, sess, "CREATE TABLE vip (customer INT PRIMARY KEY)")
	for i := 1; i <= 5; i++ {
		run(t, ex, sess, fmt.Sprintf("INSERT INTO orders (id, customer) VALUES (%d, %d)", i, i))
	}
	run(t, ex, sess, "INSERT INTO vip (customer) VALUES (2)")
	run(t, ex, sess, "INSERT INTO vip (customer) VALUES (4)")

	r := run(t, ex, sess, "SELECT id FROM orders WHERE customer IN (SELECT customer FROM vip)")
	if len(r.Rows) != 2 {
		t.Fatalf("IN subquery want 2 rows (customers 2,4), got %d", len(r.Rows))
	}
}

func TestScalarSubquery(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY, n INT)")
	for _, n := range []int{5, 12, 30, 7} {
		run(t, ex, sess, fmt.Sprintf("INSERT INTO t (id, n) VALUES (%d, %d)", n, n))
	}
	// Rows with n greater than the average.
	r := run(t, ex, sess, "SELECT id FROM t WHERE n > (SELECT AVG(n) FROM t)")
	// avg = (5+12+30+7)/4 = 13.5 → rows with n=30 only
	if len(r.Rows) != 1 || r.Rows[0][0].Text != "30" {
		t.Fatalf("scalar subquery (n > avg) want [30], got %#v", r.Rows)
	}
}
