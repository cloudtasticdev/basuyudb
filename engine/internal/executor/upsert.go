package executor

import (
	"context"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// handleOnConflict applies an INSERT ... ON CONFLICT action when the primary key
// already exists. DO NOTHING leaves the row untouched; DO UPDATE SET applies the
// assignments to the existing row (column names resolve to the existing row,
// EXCLUDED.<col> to the proposed insert row), maintaining indexes.
func (e *execImpl) handleOnConflict(ctx context.Context, txn *transactions.Txn, owns bool, sess *session.Session, sch *tableSchema, table string, rowKey storage.Key, proposed []Datum, existingRaw []byte, s *ast.InsertStmt, params []Datum) (*Result, error) {
	oc := s.OnConflict

	if oc.DoNothing {
		// No mutation; INSERT affected 0 rows.
		if err := e.commitTx(ctx, txn, owns); err != nil {
			return nil, err
		}
		if len(s.ReturningList) > 0 {
			return projectReturning(sch, table, s.ReturningList, nil, params)
		}
		return &Result{Command: "INSERT", RowsAffected: 0}, nil
	}

	// DO UPDATE SET.
	existing, err := decodeRow(existingRaw, len(sch.Cols))
	if err != nil {
		return nil, err
	}
	newCells := append([]Datum(nil), existing...)

	resolve := func(fields []string) (value, error) {
		col := fields[len(fields)-1]
		// EXCLUDED.<col> refers to the row proposed by the INSERT.
		if len(fields) > 1 && strings.EqualFold(fields[len(fields)-2], "excluded") {
			idx := sch.colIndex(col)
			if idx < 0 {
				return value{}, newExecError("42703", "column %q does not exist", col)
			}
			c := proposed[idx]
			return value{null: c.Null, text: c.Text, oid: sch.Cols[idx].TypeOID}, nil
		}
		idx := sch.colIndex(col)
		if idx < 0 {
			return value{}, newExecError("42703", "column %q does not exist", col)
		}
		c := newCells[idx]
		return value{null: c.Null, text: c.Text, oid: sch.Cols[idx].TypeOID}, nil
	}

	ev := &evaluator{params: params, resolveCol: resolve}
	for _, set := range oc.DoUpdateSet {
		idx := sch.colIndex(set.Name)
		if idx < 0 {
			return nil, newExecError("42703", "column %q does not exist", set.Name)
		}
		v, err := ev.eval(set.Val)
		if err != nil {
			return nil, err
		}
		newCells[idx] = Datum{Null: v.null, Text: v.text}
	}

	defs, err := e.loadIndexes(ctx, txn, sess, table)
	if err != nil {
		return nil, err
	}
	if err := e.indexEntries(txn, sess, sch, defs, existing, rowKey, false); err != nil {
		return nil, err
	}
	e.txn.Buffer(txn, transactions.Mutation{Key: rowKey, Value: encodeRow(newCells)})
	if err := e.indexEntries(txn, sess, sch, defs, newCells, rowKey, true); err != nil {
		return nil, err
	}

	result := &Result{Command: "INSERT", RowsAffected: 1}
	if len(s.ReturningList) > 0 {
		rr, err := projectReturning(sch, table, s.ReturningList, [][]Datum{newCells}, params)
		if err != nil {
			return nil, err
		}
		rr.Command, rr.RowsAffected = "INSERT", 1
		result = rr
	}
	if err := e.commitTx(ctx, txn, owns); err != nil {
		return nil, err
	}
	return result, nil
}
