package executor

import (
	"context"
	"fmt"
	"testing"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/parser"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
)

// indexScanProbeParams is indexScanProbe but threads bound parameters into the
// planner, so a `<expr> = $1` / `col = $1` query can be tested for index use.
func (e *execImpl) indexScanProbeParams(t *testing.T, sess *session.Session, sql string, params []Datum) ([]boundRow, bool, error) {
	t.Helper()
	stmt, err := parser.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	sel := stmt.(*ast.SelectStmt)
	txn, err := e.txn.Begin(context.Background(), sess.Auth)
	if err != nil {
		return nil, false, err
	}
	defer e.txn.Rollback(context.Background(), txn)
	scan, err := e.planIndexScan(context.Background(), txn, sess, sel, params)
	if err != nil {
		return nil, false, err
	}
	if scan == nil {
		return nil, false, nil
	}
	return scan.rows, true, nil
}

// TestExpressionIndexUsedForBoundParam proves an expression index on
// lower(email) is used for `WHERE lower(email) = $1` when $1 is bound to a
// constant at scan time, and that an unbound/NULL param falls back to seq scan.
func TestExpressionIndexUsedForBoundParam(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE users (id INT PRIMARY KEY, email TEXT)")
	for i := 1; i <= 20; i++ {
		run(t, ex, sess, fmt.Sprintf("INSERT INTO users (id, email) VALUES (%d, 'User%d@X.io')", i, i))
	}
	run(t, ex, sess, "CREATE INDEX idx_lower_email ON users (lower(email))")

	// End-to-end SELECT with a bound param returns the right row.
	res := run(t, ex, sess, "SELECT id FROM users WHERE lower(email) = $1", Datum{Text: "user7@x.io"})
	if len(res.Rows) != 1 || res.Rows[0][0].Text != "7" {
		t.Fatalf("bound-param expr lookup want id 7, got %+v", res.Rows)
	}

	// The expression-index fast path must be taken when $1 is bound.
	rows, used, err := ex.(*execImpl).indexScanProbeParams(t, sess,
		"SELECT id FROM users WHERE lower(email) = $1", []Datum{{Text: "user3@x.io"}})
	if err != nil {
		t.Fatal(err)
	}
	if !used {
		t.Fatal("expected expression index to be used for lower(email) = $1 with $1 bound")
	}
	if len(rows) != 1 {
		t.Fatalf("bound-param expr index probe want 1 row, got %d", len(rows))
	}

	// A NULL-bound param is not resolvable → no index acceleration (seq fallback).
	if _, used, err := ex.(*execImpl).indexScanProbeParams(t, sess,
		"SELECT id FROM users WHERE lower(email) = $1", []Datum{{Null: true}}); err != nil {
		t.Fatal(err)
	} else if used {
		t.Fatal("NULL-bound param must not drive the expression index")
	}
}

// TestPartialIndexUsedForBoundParam proves the single-column partial-index path
// also accepts a bound parameter as the equality constant.
func TestPartialIndexUsedForBoundParam(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE accounts (id INT PRIMARY KEY, email TEXT, active BOOL)")
	for i := 1; i <= 20; i++ {
		active := "true"
		if i%2 == 0 {
			active = "false"
		}
		run(t, ex, sess, fmt.Sprintf("INSERT INTO accounts (id, email, active) VALUES (%d, 'a%d@x.io', %s)", i, i, active))
	}
	run(t, ex, sess, "CREATE INDEX idx_active_email ON accounts (email) WHERE active")

	// Q = `active AND email = $1`, $1 bound → implies P=active, index usable.
	rows, used, err := ex.(*execImpl).indexScanProbeParams(t, sess,
		"SELECT id FROM accounts WHERE active AND email = $1", []Datum{{Text: "a5@x.io"}})
	if err != nil {
		t.Fatal(err)
	}
	if !used {
		t.Fatal("expected partial index to be used for email = $1 with $1 bound")
	}
	if len(rows) != 1 {
		t.Fatalf("partial bound-param probe want 1 row, got %d", len(rows))
	}

	// End-to-end correctness with the bound param.
	res := run(t, ex, sess, "SELECT id FROM accounts WHERE active AND email = $1", Datum{Text: "a5@x.io"})
	if len(res.Rows) != 1 || res.Rows[0][0].Text != "5" {
		t.Fatalf("bound-param partial lookup want id 5, got %+v", res.Rows)
	}
}
