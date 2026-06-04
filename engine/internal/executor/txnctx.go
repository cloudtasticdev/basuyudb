package executor

import (
	"context"
	"errors"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// txnCtxKey carries an explicit (multi-statement) transaction on a context.
type txnCtxKey struct{}

// CtxWithTxn returns a context carrying an explicit transaction. Statements run
// with this context join the transaction (read-your-writes; no per-statement
// commit) instead of autocommitting. The PG wire layer sets it between BEGIN
// and COMMIT/ROLLBACK.
func CtxWithTxn(ctx context.Context, tx *transactions.Txn) context.Context {
	return context.WithValue(ctx, txnCtxKey{}, tx)
}

func txnFromCtx(ctx context.Context) *transactions.Txn {
	tx, _ := ctx.Value(txnCtxKey{}).(*transactions.Txn)
	return tx
}

// beginTx joins the ambient explicit transaction when one is active on the
// context (owns=false), otherwise begins a fresh autocommit transaction
// (owns=true). Only the owner commits/rolls back.
func (e *execImpl) beginTx(ctx context.Context, sess auth.Session) (*transactions.Txn, bool, error) {
	if tx := txnFromCtx(ctx); tx != nil {
		return tx, false, nil
	}
	tx, err := e.txn.Begin(ctx, sess)
	return tx, true, err
}

// commitTx commits an owned autocommit transaction; an ambient explicit
// transaction commits later at COMMIT.
func (e *execImpl) commitTx(ctx context.Context, tx *transactions.Txn, owns bool) error {
	if owns {
		return e.txn.Commit(ctx, tx)
	}
	return nil
}

// rollbackTx rolls back an owned transaction; an ambient explicit transaction
// is left open for subsequent statements (it rolls back only on ROLLBACK or a
// statement error handled by the wire layer).
func (e *execImpl) rollbackTx(ctx context.Context, tx *transactions.Txn, owns bool) {
	if owns {
		_ = e.txn.Rollback(ctx, tx)
	}
}

// BeginExplicit starts a multi-statement transaction (the wire layer's BEGIN).
func (e *execImpl) BeginExplicit(ctx context.Context, sess *session.Session) (*transactions.Txn, error) {
	return e.txn.Begin(ctx, sess.Auth)
}

// CommitExplicit commits a multi-statement transaction (the wire layer's COMMIT).
// Before committing it runs every DEFERRABLE constraint check deferred during
// the transaction; if any fails the commit is aborted (the transaction is rolled
// back) and the FK error (23503) is returned. A write-write conflict surfaces as
// SQLSTATE 40001 so the client retries the whole transaction (first-committer-
// wins snapshot isolation).
func (e *execImpl) CommitExplicit(ctx context.Context, tx *transactions.Txn, sess *session.Session) error {
	if st := e.deferred.peek(tx); st != nil && len(st.pending) > 0 {
		// Re-run the deferred checks against the transaction's final snapshot
		// (read-your-writes within tx). A surviving violation aborts the commit.
		if err := e.runPendingChecks(ctx, tx, sess, st, nil); err != nil {
			_ = e.txn.Rollback(ctx, tx)
			e.deferred.clear(tx)
			return err
		}
	}
	if err := e.txn.Commit(ctx, tx); err != nil {
		e.deferred.clear(tx)
		if errors.Is(err, transactions.ErrWriteConflict) {
			return newExecError("40001", "could not serialize access due to concurrent update")
		}
		return err
	}
	e.deferred.clear(tx)
	return nil
}

// RollbackExplicit rolls back a multi-statement transaction (ROLLBACK) and
// discards its deferred-constraint state.
func (e *execImpl) RollbackExplicit(ctx context.Context, tx *transactions.Txn) error {
	e.deferred.clear(tx)
	return e.txn.Rollback(ctx, tx)
}
