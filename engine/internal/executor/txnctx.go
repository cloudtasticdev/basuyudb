package executor

import (
	"context"

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
func (e *execImpl) CommitExplicit(ctx context.Context, tx *transactions.Txn) error {
	return e.txn.Commit(ctx, tx)
}

// RollbackExplicit rolls back a multi-statement transaction (ROLLBACK).
func (e *execImpl) RollbackExplicit(ctx context.Context, tx *transactions.Txn) error {
	return e.txn.Rollback(ctx, tx)
}
