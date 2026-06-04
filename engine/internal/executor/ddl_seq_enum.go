package executor

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// enumKey returns the namespace-scoped key for a stored enum type.
func (e *execImpl) enumKey(sess *session.Session, name string) storage.Key {
	return e.store.Encoder().SchemaKey(sess.Namespace(), "#enum#"+name)
}

// execCreateSequence persists a new sequence counter at value 0.
// If the sequence already exists and IfNotExists is true, it is a no-op.
func (e *execImpl) execCreateSequence(ctx context.Context, s *ast.CreateSeqStmt, sess *session.Session) (*Result, error) {
	name := s.Sequence.RelName

	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	key := e.seqKey(sess, name)
	_, getErr := e.txn.Get(ctx, txn, key)
	if getErr == nil {
		// Sequence already exists.
		if s.IfNotExists {
			return &Result{Command: "CREATE SEQUENCE"}, nil
		}
		return nil, newExecError("42P07", "sequence %q already exists", name)
	}
	if !errors.Is(getErr, storage.ErrKeyNotFound) {
		return nil, getErr
	}

	// Store initial value of 0 (nextSequenceVal increments before returning).
	e.txn.Buffer(txn, transactions.Mutation{Key: key, Value: []byte(strconv.FormatInt(0, 10))})
	if err := e.commitTx(ctx, txn, owns); err != nil {
		return nil, err
	}
	return &Result{Command: "CREATE SEQUENCE"}, nil
}

// execDropSequence removes a sequence counter.
func (e *execImpl) execDropSequence(ctx context.Context, s *ast.DropStmt, sess *session.Session) (*Result, error) {
	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	key := e.seqKey(sess, s.Table)
	_, getErr := e.txn.Get(ctx, txn, key)
	if getErr != nil {
		if errors.Is(getErr, storage.ErrKeyNotFound) {
			if s.IfExists {
				return &Result{Command: "DROP SEQUENCE"}, nil
			}
			return nil, newExecError("42P01", "sequence %q does not exist", s.Table)
		}
		return nil, getErr
	}

	e.txn.Buffer(txn, transactions.Mutation{Key: key, Delete: true})
	if err := e.commitTx(ctx, txn, owns); err != nil {
		return nil, err
	}
	return &Result{Command: "DROP SEQUENCE"}, nil
}

// execCreateEnum persists an enum type's ordered labels under "#enum#name".
func (e *execImpl) execCreateEnum(ctx context.Context, s *ast.CreateEnumStmt, sess *session.Session) (*Result, error) {
	name := s.TypeName.RelName

	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	key := e.enumKey(sess, name)
	val := strings.Join(s.Vals, ",")
	e.txn.Buffer(txn, transactions.Mutation{Key: key, Value: []byte(val)})
	if err := e.commitTx(ctx, txn, owns); err != nil {
		return nil, err
	}
	return &Result{Command: "CREATE TYPE"}, nil
}
