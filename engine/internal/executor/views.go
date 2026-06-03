package executor

import (
	"context"
	"errors"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/parser"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// viewKey is the namespace-scoped metadata key holding a view's deparsed SELECT
// text (beside schema/index/sequence metadata, under a "#view#" discriminator).
func (e *execImpl) viewKey(sess *session.Session, name string) storage.Key {
	return e.store.Encoder().SchemaKey(sess.Namespace(), "#view#"+name)
}

// execCreateView deparses the view's query to SQL, validates it round-trips, and
// persists it. The view is materialized on each reference (no stored data).
func (e *execImpl) execCreateView(ctx context.Context, c *ast.CreateViewStmt, sess *session.Session) (*Result, error) {
	name := c.Relation.RelName
	sel, ok := c.Query.(*ast.SelectStmt)
	if !ok {
		return nil, newExecError("0A000", "view query must be a SELECT")
	}
	sql, err := deparseSelect(sel)
	if err != nil {
		return nil, err
	}
	// Round-trip guard: the deparsed text must parse back to a statement.
	if _, perr := parser.Parse(sql); perr != nil {
		return nil, newExecError("XX000", "could not persist view definition: %v", perr)
	}

	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	// A view may not shadow a real table.
	if _, err := e.loadSchema(ctx, txn, sess, name); err == nil {
		return nil, newExecError("42P07", "relation %q already exists", name)
	}
	vkey := e.viewKey(sess, name)
	if !c.Replace {
		if _, err := e.txn.Get(ctx, txn, vkey); err == nil {
			return nil, newExecError("42P07", "relation %q already exists", name)
		} else if !errors.Is(err, storage.ErrKeyNotFound) {
			return nil, err
		}
	}
	e.txn.Buffer(txn, transactions.Mutation{Key: vkey, Value: []byte(sql)})
	if err := e.commitTx(ctx, txn, owns); err != nil {
		return nil, err
	}
	return &Result{Command: "CREATE VIEW"}, nil
}

// execDropView removes a persisted view definition.
func (e *execImpl) execDropView(ctx context.Context, s *ast.DropStmt, sess *session.Session) (*Result, error) {
	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	vkey := e.viewKey(sess, s.Table)
	if _, err := e.txn.Get(ctx, txn, vkey); err != nil {
		if errors.Is(err, storage.ErrKeyNotFound) {
			if s.IfExists {
				return &Result{Command: "DROP VIEW"}, nil
			}
			return nil, newExecError("42P01", "view %q does not exist", s.Table)
		}
		return nil, err
	}
	e.txn.Buffer(txn, transactions.Mutation{Key: vkey, Delete: true})
	if err := e.commitTx(ctx, txn, owns); err != nil {
		return nil, err
	}
	return &Result{Command: "DROP VIEW"}, nil
}

// loadViewSQL returns a view's stored SELECT text, if a view by that name exists.
func (e *execImpl) loadViewSQL(ctx context.Context, txn *transactions.Txn, sess *session.Session, name string) (string, bool, error) {
	raw, err := e.txn.Get(ctx, txn, e.viewKey(sess, name))
	if errors.Is(err, storage.ErrKeyNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(raw), true, nil
}

// materializeView parses a view's stored query, executes it, and binds the result
// rows under the reference alias so the FROM path treats it like a table.
func (e *execImpl) materializeView(ctx context.Context, sess *session.Session, alias, sql string) ([]boundRow, error) {
	stmt, err := parser.Parse(sql)
	if err != nil {
		return nil, newExecError("XX000", "corrupt view definition %q: %v", alias, err)
	}
	sel, ok := stmt.(*ast.SelectStmt)
	if !ok {
		return nil, newExecError("XX000", "view definition is not a SELECT")
	}
	res, err := e.execSelect(ctx, sel, sess, nil)
	if err != nil {
		return nil, err
	}
	sch := &tableSchema{Name: alias, PKIndex: -1}
	for _, col := range res.Columns {
		sch.Cols = append(sch.Cols, colMeta{Name: col.Name, TypeOID: col.TypeOID})
	}
	rows := make([]boundRow, 0, len(res.Rows))
	for _, cells := range res.Rows {
		rows = append(rows, boundRow{{alias: alias, schema: sch, cells: cells}})
	}
	return rows, nil
}
