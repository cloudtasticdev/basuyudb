package executor

import (
	"context"
	"encoding/json"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// isRelMissing reports whether err is a "relation does not exist" (42P01).
func isRelMissing(err error) bool {
	if ee, ok := err.(*ExecError); ok {
		return ee.SQLSTATE == "42P01"
	}
	return false
}

// deletePrefix buffers a delete for every committed key under prefix on the
// session's branch (the iterator reads the snapshot, deletes go to the buffer).
func (e *execImpl) deletePrefix(txn *transactions.Txn, prefix storage.Key) {
	it := e.txn.NewIterator(txn, prefix)
	defer it.Close()
	for it.Rewind(); it.Valid(); it.Next() {
		e.txn.Buffer(txn, transactions.Mutation{Key: it.Key(), Delete: true})
	}
}

// dropTableData removes all rows and index entries of a table on the session's
// branch (shared by DROP TABLE and TRUNCATE).
func (e *execImpl) dropTableData(ctx context.Context, txn *transactions.Txn, sess *session.Session, table string) error {
	enc := e.store.Encoder()
	e.deletePrefix(txn, enc.RowPrefix(sess.Namespace(), sess.Branch(), table))
	defs, err := e.loadIndexes(ctx, txn, sess, table)
	if err != nil {
		return err
	}
	for _, d := range defs {
		e.deletePrefix(txn, enc.IndexColumnPrefix(sess.Namespace(), sess.Branch(), d.Table, d.Column))
	}
	return nil
}

// execDropTable removes a table's schema, index definitions, rows, and index
// entries. DROP TABLE IF EXISTS is a no-op when the table is absent.
func (e *execImpl) execDropTable(ctx context.Context, s *ast.DropStmt, sess *session.Session) (*Result, error) {
	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	if _, err := e.loadSchema(ctx, txn, sess, s.Table); err != nil {
		if s.IfExists && isRelMissing(err) {
			return &Result{Command: "DROP TABLE"}, nil
		}
		return nil, err
	}
	if err := e.dropTableData(ctx, txn, sess, s.Table); err != nil {
		return nil, err
	}
	enc := e.store.Encoder()
	e.txn.Buffer(txn, transactions.Mutation{Key: e.indexListKey(sess, s.Table), Delete: true})
	e.txn.Buffer(txn, transactions.Mutation{Key: enc.SchemaKey(sess.Namespace(), s.Table), Delete: true})
	if err := e.commitTx(ctx, txn, owns); err != nil {
		return nil, err
	}
	return &Result{Command: "DROP TABLE"}, nil
}

// execTruncate removes all rows and index entries but keeps the schema and
// index definitions.
func (e *execImpl) execTruncate(ctx context.Context, s *ast.TruncateStmt, sess *session.Session) (*Result, error) {
	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	if _, err := e.loadSchema(ctx, txn, sess, s.Table); err != nil {
		return nil, err
	}
	if err := e.dropTableData(ctx, txn, sess, s.Table); err != nil {
		return nil, err
	}
	if err := e.commitTx(ctx, txn, owns); err != nil {
		return nil, err
	}
	return &Result{Command: "TRUNCATE TABLE"}, nil
}

// execAlterTable applies ADD COLUMN / DROP COLUMN to a table's schema. ADD is
// online (existing rows read the new column as NULL via decodeRow padding).
// DROP rewrites every row without the column and removes any index on it.
func (e *execImpl) execAlterTable(ctx context.Context, s *ast.AlterTableStmt, sess *session.Session) (*Result, error) {
	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	sch, err := e.loadSchema(ctx, txn, sess, s.Table)
	if err != nil {
		return nil, err
	}

	switch s.Kind {
	case ast.AlterAddColumn:
		if sch.colIndex(s.Column.ColName) >= 0 {
			return nil, newExecError("42701", "column %q of relation %q already exists", s.Column.ColName, s.Table)
		}
		if s.Column.PrimaryKey {
			return nil, newExecError("0A000", "ADD COLUMN ... PRIMARY KEY is not supported")
		}
		sch.Cols = append(sch.Cols, colMeta{
			Name:    s.Column.ColName,
			TypeOID: oidForTypeName(s.Column.TypeName),
			NotNull: s.Column.NotNull,
		})

	case ast.AlterDropColumn:
		idx := sch.colIndex(s.Column.ColName)
		if idx < 0 {
			return nil, newExecError("42703", "column %q of relation %q does not exist", s.Column.ColName, s.Table)
		}
		if sch.PKIndex == idx {
			return nil, newExecError("0A000", "cannot drop primary key column %q", s.Column.ColName)
		}
		// Rewrite each row without the dropped cell.
		sc, err := e.scanTable(ctx, txn, sess, s.Table, s.Table)
		if err != nil {
			return nil, err
		}
		for _, r := range sc.rows {
			newCells := make([]Datum, 0, len(r.cells)-1)
			for i, c := range r.cells {
				if i != idx {
					newCells = append(newCells, c)
				}
			}
			e.txn.Buffer(txn, transactions.Mutation{Key: r.key, Value: encodeRow(newCells)})
		}
		// Drop any index on the column and its entries.
		defs, err := e.loadIndexes(ctx, txn, sess, s.Table)
		if err != nil {
			return nil, err
		}
		kept := defs[:0]
		for _, d := range defs {
			if d.Column == s.Column.ColName {
				e.deletePrefix(txn, e.store.Encoder().IndexColumnPrefix(sess.Namespace(), sess.Branch(), d.Table, d.Column))
				continue
			}
			kept = append(kept, d)
		}
		if len(kept) != len(defs) {
			raw, _ := json.Marshal(kept)
			e.txn.Buffer(txn, transactions.Mutation{Key: e.indexListKey(sess, s.Table), Value: raw})
		}
		// Remove the column; shift PKIndex if it followed the dropped column.
		sch.Cols = append(sch.Cols[:idx], sch.Cols[idx+1:]...)
		if sch.PKIndex > idx {
			sch.PKIndex--
		}
	}

	raw, err := json.Marshal(sch)
	if err != nil {
		return nil, newExecError("XX000", "encode schema: %v", err)
	}
	e.txn.Buffer(txn, transactions.Mutation{Key: e.store.Encoder().SchemaKey(sess.Namespace(), s.Table), Value: raw})
	if err := e.commitTx(ctx, txn, owns); err != nil {
		return nil, err
	}
	return &Result{Command: "ALTER TABLE"}, nil
}
