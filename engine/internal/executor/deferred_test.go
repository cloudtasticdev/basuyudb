package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudtasticdev/basuyudb/engine/internal/parser"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// fkErr reports whether err is a foreign-key violation (23503).
func fkErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "23503") || strings.Contains(err.Error(), "foreign key")
}

// beginTxn opens an explicit transaction and returns it with a txn-scoped ctx.
func beginTxn(t *testing.T, ex Executor, sess *session.Session) (*transactions.Txn, context.Context) {
	t.Helper()
	tx, err := ex.BeginExplicit(context.Background(), sess)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	return tx, CtxWithTxn(context.Background(), tx)
}

// TestDeferredFKInitiallyDeferredReorder covers (a): with a DEFERRABLE INITIALLY
// DEFERRED FK, a child row may be inserted before its parent within one
// transaction; COMMIT succeeds once the parent exists.
func TestDeferredFKInitiallyDeferredReorder(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE parent (id INT PRIMARY KEY)")
	run(t, ex, sess, "CREATE TABLE child (id INT PRIMARY KEY, pid INT REFERENCES parent(id) DEFERRABLE INITIALLY DEFERRED)")

	tx, ctx := beginTxn(t, ex, sess)
	// Child inserted BEFORE the parent exists — would fail if checked immediately.
	exec(t, ex, ctx, sess, "INSERT INTO child (id, pid) VALUES (1, 100)")
	exec(t, ex, ctx, sess, "INSERT INTO parent (id) VALUES (100)")
	if err := ex.CommitExplicit(context.Background(), tx, sess); err != nil {
		t.Fatalf("commit should succeed once parent present, got %v", err)
	}

	// The rows persisted.
	if got := len(exec(t, ex, context.Background(), sess, "SELECT * FROM child").Rows); got != 1 {
		t.Fatalf("want 1 child row after commit, got %d", got)
	}
}

// TestDeferredFKCommitViolation covers (b): a DEFERRABLE INITIALLY DEFERRED FK
// child with no parent passes during the txn but fails at COMMIT (23503), and
// the transaction does not persist.
func TestDeferredFKCommitViolation(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE parent (id INT PRIMARY KEY)")
	run(t, ex, sess, "CREATE TABLE child (id INT PRIMARY KEY, pid INT REFERENCES parent(id) DEFERRABLE INITIALLY DEFERRED)")

	tx, ctx := beginTxn(t, ex, sess)
	// Passes now (deferred), parent never inserted.
	exec(t, ex, ctx, sess, "INSERT INTO child (id, pid) VALUES (1, 999)")
	err := ex.CommitExplicit(context.Background(), tx, sess)
	if !fkErr(err) {
		t.Fatalf("commit should fail with FK violation, got %v", err)
	}

	// Aborted: nothing persisted.
	if got := len(exec(t, ex, context.Background(), sess, "SELECT * FROM child").Rows); got != 0 {
		t.Fatalf("want 0 child rows after aborted commit, got %d", got)
	}
}

// TestSetConstraintsAllDeferred covers (c): an INITIALLY IMMEDIATE but DEFERRABLE
// FK is made deferred for the txn via SET CONSTRAINTS ALL DEFERRED, allowing the
// child-before-parent ordering.
func TestSetConstraintsAllDeferred(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE parent (id INT PRIMARY KEY)")
	run(t, ex, sess, "CREATE TABLE child (id INT PRIMARY KEY, pid INT REFERENCES parent(id) DEFERRABLE INITIALLY IMMEDIATE)")

	tx, ctx := beginTxn(t, ex, sess)
	// Without deferral this insert would fail immediately; defer ALL first.
	if err := ex.SetConstraints(ctx, tx, sess, true, nil, true); err != nil {
		t.Fatalf("SET CONSTRAINTS ALL DEFERRED: %v", err)
	}
	exec(t, ex, ctx, sess, "INSERT INTO child (id, pid) VALUES (1, 7)")
	exec(t, ex, ctx, sess, "INSERT INTO parent (id) VALUES (7)")
	if err := ex.CommitExplicit(context.Background(), tx, sess); err != nil {
		t.Fatalf("commit should succeed, got %v", err)
	}
}

// TestSetConstraintsImmediateRechecks covers (d): SET CONSTRAINTS ... IMMEDIATE
// re-checks pending deferred violations and errors (23503) when one fails.
func TestSetConstraintsImmediateRechecks(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE parent (id INT PRIMARY KEY)")
	run(t, ex, sess, "CREATE TABLE child (id INT PRIMARY KEY, pid INT REFERENCES parent(id) DEFERRABLE INITIALLY DEFERRED)")

	tx, ctx := beginTxn(t, ex, sess)
	// Deferred by default — insert with a missing parent passes for now.
	exec(t, ex, ctx, sess, "INSERT INTO child (id, pid) VALUES (1, 555)")
	// Switching to IMMEDIATE must re-check and surface the pending violation.
	err := ex.SetConstraints(ctx, tx, sess, true, nil, false)
	if !fkErr(err) {
		t.Fatalf("SET CONSTRAINTS ALL IMMEDIATE should re-check and fail, got %v", err)
	}
	_ = ex.RollbackExplicit(context.Background(), tx)
}

// TestNonDeferrableFKStillImmediate covers (e): a non-deferrable FK is enforced
// immediately even inside an explicit transaction (regression guard — deferral
// must never weaken default enforcement).
func TestNonDeferrableFKStillImmediate(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE parent (id INT PRIMARY KEY)")
	run(t, ex, sess, "CREATE TABLE child (id INT PRIMARY KEY, pid INT REFERENCES parent(id))")

	tx, ctx := beginTxn(t, ex, sess)
	// Even SET CONSTRAINTS ALL DEFERRED cannot defer a non-deferrable FK.
	if err := ex.SetConstraints(ctx, tx, sess, true, nil, true); err != nil {
		t.Fatalf("SET CONSTRAINTS: %v", err)
	}
	stmt, perr := parser.Parse("INSERT INTO child (id, pid) VALUES (1, 42)")
	if perr != nil {
		t.Fatalf("parse: %v", perr)
	}
	_, err := ex.Execute(ctx, stmt, sess, nil)
	if !fkErr(err) {
		t.Fatalf("non-deferrable FK must fail immediately, got %v", err)
	}
	_ = ex.RollbackExplicit(context.Background(), tx)
}
