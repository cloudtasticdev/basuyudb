package executor

import (
	"context"
	"testing"

	"github.com/cloudtasticdev/basuyudb/engine/internal/parser"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
)

func exec(t *testing.T, ex Executor, ctx context.Context, sess *session.Session, sql string) *Result {
	t.Helper()
	stmt, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	res, err := ex.Execute(ctx, stmt, sess, nil)
	if err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
	return res
}

// TestTransactionCommitRollback verifies multi-statement atomicity: a rolled-back
// transaction leaves no trace; a committed one persists all statements.
func TestTransactionCommitRollback(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)
	bg := context.Background()
	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY, n INT)")

	// ROLLBACK: two inserts, then discard — table stays empty.
	tx, err := ex.BeginExplicit(bg, sess)
	if err != nil {
		t.Fatal(err)
	}
	txCtx := CtxWithTxn(bg, tx)
	exec(t, ex, txCtx, sess, "INSERT INTO t (id, n) VALUES (1, 10)")
	exec(t, ex, txCtx, sess, "INSERT INTO t (id, n) VALUES (2, 20)")
	if err := ex.RollbackExplicit(bg, tx); err != nil {
		t.Fatal(err)
	}
	if got := len(exec(t, ex, bg, sess, "SELECT * FROM t").Rows); got != 0 {
		t.Fatalf("after rollback table must be empty, got %d rows", got)
	}

	// COMMIT: two inserts persist atomically.
	tx2, _ := ex.BeginExplicit(bg, sess)
	tx2Ctx := CtxWithTxn(bg, tx2)
	exec(t, ex, tx2Ctx, sess, "INSERT INTO t (id, n) VALUES (3, 30)")
	exec(t, ex, tx2Ctx, sess, "INSERT INTO t (id, n) VALUES (4, 40)")
	// Isolation: a separate autocommit read does NOT see the uncommitted rows.
	if got := len(exec(t, ex, bg, sess, "SELECT * FROM t").Rows); got != 0 {
		t.Fatalf("uncommitted rows must not be visible outside the txn, got %d", got)
	}
	if err := ex.CommitExplicit(bg, tx2); err != nil {
		t.Fatal(err)
	}
	if got := len(exec(t, ex, bg, sess, "SELECT * FROM t").Rows); got != 2 {
		t.Fatalf("after commit want 2 rows, got %d", got)
	}
}

// TestTransactionReadYourWrites verifies point-lookup read-your-writes inside a
// transaction (a PK lookup sees the txn's own uncommitted insert/update).
func TestTransactionReadYourWrites(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)
	bg := context.Background()
	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY, n INT)")

	tx, _ := ex.BeginExplicit(bg, sess)
	txCtx := CtxWithTxn(bg, tx)
	exec(t, ex, txCtx, sess, "INSERT INTO t (id, n) VALUES (1, 10)")
	// PK lookup within the txn sees the uncommitted row (read-your-writes).
	r := exec(t, ex, txCtx, sess, "SELECT n FROM t WHERE id = 1")
	if len(r.Rows) != 1 || r.Rows[0][0].Text != "10" {
		t.Fatalf("read-your-writes: want n=10 within txn, got %#v", r.Rows)
	}
	_ = ex.RollbackExplicit(bg, tx)
}
