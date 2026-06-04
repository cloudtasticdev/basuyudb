package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// rowResolver builds a column resolver closure for a scanned row. It matches an
// unqualified column name, or a name qualified by the table/alias.
func rowResolver(sch *tableSchema, alias string, cells []Datum) func(fields []string) (value, error) {
	return func(fields []string) (value, error) {
		col := fields[len(fields)-1]
		if len(fields) > 1 {
			q := fields[0]
			if !strings.EqualFold(q, alias) && !strings.EqualFold(q, sch.Name) {
				return value{}, newExecError("42P01", "missing FROM-clause entry for table %q", q)
			}
		}
		idx := sch.colIndex(col)
		if idx < 0 {
			return value{}, newExecError("42703", "column %q does not exist", col)
		}
		c := cells[idx]
		return value{null: c.Null, text: c.Text, oid: sch.Cols[idx].TypeOID}, nil
	}
}

// execInsert inserts one or more rows. It dispatches to:
//   - execInsertMultiRow when MultiRows carries more than one row (Task 1)
//   - execInsertSelect for INSERT ... SELECT (Task 2)
//   - the single-row VALUES path for the common case
func (e *execImpl) execInsert(ctx context.Context, s *ast.InsertStmt, sess *session.Session, params []Datum) (*Result, error) {
	// Task 1: multi-row VALUES
	if len(s.MultiRows) > 0 {
		return e.execInsertMultiRow(ctx, s, sess, params)
	}

	// Task 2: INSERT ... SELECT (SelectStmt with a real FROM / set-op / WITH)
	if sel, ok := s.SelectStmt.(*ast.SelectStmt); ok {
		if len(sel.FromClause) > 0 || sel.SetOp != ast.SetOpNone || sel.WithClause != nil {
			return e.execInsertSelect(ctx, s, sess, params)
		}
	}

	table := s.Relation.RelName

	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	sch, err := e.resolveSchema(ctx, txn, sess, table)
	if err != nil {
		return nil, err
	}

	// Evaluate the VALUES expressions (constants/params only at this milestone).
	valSel, ok := s.SelectStmt.(*ast.SelectStmt)
	if !ok || len(valSel.TargetList) == 0 {
		return nil, newExecError("0A000", "only INSERT ... VALUES (...) is supported")
	}
	ev := &evaluator{params: params}
	provided := make([]value, len(valSel.TargetList))
	isDefault := make([]bool, len(valSel.TargetList))
	for i, t := range valSel.TargetList {
		// DEFAULT keyword as a value (ORMs emit it): defer to the column default.
		if _, ok := t.Val.(*ast.SetToDefault); ok {
			isDefault[i] = true
			continue
		}
		v, err := ev.eval(t.Val)
		if err != nil {
			return nil, err
		}
		provided[i] = v
	}

	// Map provided values to columns. Either an explicit column list or all
	// columns in declared order.
	targetCols := make([]int, 0, len(sch.Cols))
	if len(s.Cols) > 0 {
		for _, c := range s.Cols {
			idx := sch.colIndex(c.Name)
			if idx < 0 {
				return nil, newExecError("42703", "column %q of relation %q does not exist", c.Name, table)
			}
			targetCols = append(targetCols, idx)
		}
	} else {
		for i := range sch.Cols {
			targetCols = append(targetCols, i)
		}
	}
	if len(targetCols) != len(provided) {
		return nil, newExecError("42601", "INSERT has %d target columns but %d values", len(targetCols), len(provided))
	}

	// Assemble the full row in column order, defaulting unspecified cols to NULL.
	cells := make([]Datum, len(sch.Cols))
	for i := range cells {
		cells[i] = Datum{Null: true}
	}
	providedSet := make(map[int]bool, len(targetCols))
	for j, colIdx := range targetCols {
		// A DEFAULT-valued column is left unprovided so the column-default pass
		// (serial/now()/literal/NULL) fills it.
		if isDefault[j] {
			continue
		}
		v := provided[j]
		cells[colIdx] = Datum{Null: v.null, Text: v.text}
		providedSet[colIdx] = true
	}

	// Column DEFAULTs (incl. SERIAL sequences, now(), gen_random_uuid()) for any
	// column not named in the INSERT. Applied before key computation so a serial
	// or defaulted primary key is materialized in time.
	for i, c := range sch.Cols {
		if providedSet[i] || c.Default == nil {
			continue
		}
		d, err := e.evalDefault(ctx, txn, sess, c.Default)
		if err != nil {
			return nil, err
		}
		cells[i] = d
	}

	// GENERATED ... AS (expr) STORED: compute from the assembled row, overriding
	// any user-supplied value.
	if err := e.applyGeneratedColumns(sch, table, cells, params); err != nil {
		return nil, err
	}

	// NOT NULL enforcement.
	for i, c := range sch.Cols {
		if c.NotNull && cells[i].Null {
			return nil, newExecError("23502", "null value in column %q violates not-null constraint", c.Name)
		}
	}

	// CHECK and FOREIGN KEY (child-side) constraint enforcement.
	if err := e.enforceChecks(sch, table, cells, params); err != nil {
		return nil, err
	}
	if err := e.enforceFKChild(ctx, txn, sess, sch, cells); err != nil {
		return nil, err
	}

	// Row-Level Security WITH CHECK on the new row (INSERT command).
	if rlsApplies(sch, sess) {
		if err := e.rlsCheckNewRow(sch, sess, "INSERT", params, rowResolver(sch, table, cells)); err != nil {
			return nil, err
		}
	}

	// Compute the row key. otel_spans is keyed by (trace_id, span_id) via
	// OtelSpanKey so SQL-inserted and OTLP-ingested spans share one key space.
	enc := e.store.Encoder()
	var rowKey storage.Key
	if table == OTelSpansTable {
		traceID := []byte(cells[0].Text) // trace_id
		spanID := []byte(cells[1].Text) // span_id
		rowKey = enc.OtelSpanKey(sess.Namespace(), sess.Branch(), traceID, spanID)
	} else {
		var pk []byte
		if sch.PKIndex >= 0 {
			if cells[sch.PKIndex].Null {
				return nil, newExecError("23502", "primary key column %q cannot be null", sch.Cols[sch.PKIndex].Name)
			}
			pk = []byte(cells[sch.PKIndex].Text)
		} else {
			pk = []byte(fmt.Sprintf("%020d", txn.ReadTimestamp()))
		}
		rowKey = enc.RowKey(sess.Namespace(), sess.Branch(), table, pk)

		// Duplicate primary key: error, or apply the ON CONFLICT action.
		if existing, err := e.txn.Get(ctx, txn, rowKey); err == nil {
			if s.OnConflict == nil {
				return nil, newExecError("23505", "duplicate key value violates unique constraint on %q", table)
			}
			return e.handleOnConflict(ctx, txn, owns, sess, sch, table, rowKey, cells, existing, s, params)
		}
	}

	// Secondary-index maintenance: enforce UNIQUE and write index entries.
	defs, err := e.loadIndexes(ctx, txn, sess, table)
	if err != nil {
		return nil, err
	}
	for _, d := range defs {
		if !d.Unique {
			continue
		}
		encTuple, ok, err := encodeIndexTuple(sch, d.Columns, cells)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // a NULL indexed column: UNIQUE treats NULLs as distinct
		}
		dup, err := e.indexHasOtherPK(ctx, txn, sess, d, encTuple, primaryKeyBytes(sch, cells, rowKey))
		if err != nil {
			return nil, err
		}
		if dup {
			return nil, newExecError("23505", "duplicate key value violates unique index %q", d.Name)
		}
	}
	e.txn.Buffer(txn, transactions.Mutation{Key: rowKey, Value: encodeRow(cells)})
	if err := e.indexEntries(txn, sess, sch, defs, cells, rowKey, true); err != nil {
		return nil, err
	}
	result := &Result{Command: "INSERT", RowsAffected: 1}
	if len(s.ReturningList) > 0 {
		rr, err := projectReturning(sch, table, s.ReturningList, [][]Datum{cells}, params)
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

// indexHasOtherPK reports whether a unique index already maps encVal (the
// memcomparable-encoded value) to a row other than pk (UNIQUE enforcement).
func (e *execImpl) indexHasOtherPK(ctx context.Context, txn *transactions.Txn, sess *session.Session, d indexDef, encVal, pk []byte) (bool, error) {
	prefix := e.store.Encoder().IndexValuePrefix(sess.Namespace(), sess.Branch(), d.Table, d.Name, encVal)
	it := e.txn.NewIterator(txn, prefix)
	defer it.Close()
	for it.Rewind(); it.Valid(); it.Next() {
		// The index entry value is the owning row's pk. A live entry whose pk
		// differs from this row's pk is a duplicate of the indexed value.
		entryPK, err := it.Value()
		if err != nil {
			return false, err
		}
		if !bytesEqual(entryPK, pk) {
			return true, nil
		}
	}
	return false, nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// branchRowKey returns the key to write on the session's branch for a row,
// computed from its primary key (so a copy-on-write write/tombstone always
// lands on the active branch, even when the matched row fell through from the
// parent). For PK-less tables it falls back to the row's existing key.
func (e *execImpl) branchRowKey(sess *session.Session, table string, sch *tableSchema, cells []Datum, existing storage.Key) storage.Key {
	if sch.PKIndex >= 0 && !cells[sch.PKIndex].Null {
		return e.store.Encoder().RowKey(sess.Namespace(), sess.Branch(), table, []byte(cells[sch.PKIndex].Text))
	}
	return existing
}

// execUpdate updates rows matching WHERE. On a feature branch writes are
// copy-on-write to the branch prefix (by design).
// When a FROM clause is present it delegates to execUpdateFrom (Task 3).
func (e *execImpl) execUpdate(ctx context.Context, s *ast.UpdateStmt, sess *session.Session, params []Datum) (*Result, error) {
	if len(s.FromClause) > 0 {
		return e.execUpdateFrom(ctx, s, sess, params)
	}

	table := s.Relation.RelName
	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	sc, err := e.scanTable(ctx, txn, sess, table, table)
	if err != nil {
		return nil, err
	}
	sch := sc.schema

	defs, err := e.loadIndexes(ctx, txn, sess, table)
	if err != nil {
		return nil, err
	}

	count := 0
	var affected [][]Datum
	for _, r := range sc.rows {
		oldCells := append([]Datum(nil), r.cells...)
		cells := append([]Datum(nil), r.cells...)
		ev := &evaluator{params: params, resolveCol: rowResolver(sch, table, cells), sess: sess}
		if s.WhereClause != nil {
			wv, err := ev.eval(s.WhereClause)
			if err != nil {
				return nil, err
			}
			if !asBool(wv) {
				continue
			}
		}
		// RLS visibility (USING) decides which rows UPDATE may target. A hidden row
		// is silently skipped (PostgreSQL does not error; it is simply not seen).
		if rlsApplies(sch, sess) {
			ok, err := e.rlsRowAllowed(sch, sess, "UPDATE", params, rowResolver(sch, table, oldCells))
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		for _, set := range s.TargetList {
			idx := sch.colIndex(set.Name)
			if idx < 0 {
				return nil, newExecError("42703", "column %q does not exist", set.Name)
			}
			v, err := ev.eval(set.Val)
			if err != nil {
				return nil, err
			}
			cells[idx] = Datum{Null: v.null, Text: v.text}
		}
		// Recompute GENERATED STORED columns from the updated row.
		if err := e.applyGeneratedColumns(sch, table, cells, params); err != nil {
			return nil, err
		}
		// NOT NULL, CHECK and FK (child-side) on the updated row.
		for i, c := range sch.Cols {
			if c.NotNull && cells[i].Null {
				return nil, newExecError("23502", "null value in column %q violates not-null constraint", c.Name)
			}
		}
		if err := e.enforceChecks(sch, table, cells, params); err != nil {
			return nil, err
		}
		if err := e.enforceFKChild(ctx, txn, sess, sch, cells); err != nil {
			return nil, err
		}
		// RLS WITH CHECK on the updated (NEW) row — the new values must satisfy the
		// UPDATE WITH CHECK predicates, else 42501.
		if rlsApplies(sch, sess) {
			if err := e.rlsCheckNewRow(sch, sess, "UPDATE", params, rowResolver(sch, table, cells)); err != nil {
				return nil, err
			}
		}
		// If this row's primary key changed and other tables reference it, apply
		// the ON UPDATE referential action (RESTRICT blocks; CASCADE/SET NULL
		// propagate to the children).
		if parentRowRefChanged(sch, oldCells, cells) {
			if err := e.enforceFKParentUpdate(ctx, txn, sess, table, sch, oldCells, cells); err != nil {
				return nil, err
			}
		}
		// Enforce UNIQUE for changed index tuples.
		for _, d := range defs {
			if !d.Unique {
				continue
			}
			newTuple, ok, err := encodeIndexTuple(sch, d.Columns, cells)
			if err != nil {
				return nil, err
			}
			oldTuple, _, _ := encodeIndexTuple(sch, d.Columns, oldCells)
			if !ok || bytesEqual(newTuple, oldTuple) {
				continue // NULL column, or tuple unchanged
			}
			dup, err := e.indexHasOtherPK(ctx, txn, sess, d, newTuple, primaryKeyBytes(sch, cells, r.key))
			if err != nil {
				return nil, err
			}
			if dup {
				return nil, newExecError("23505", "duplicate key value violates unique index %q", d.Name)
			}
		}
		bk := e.branchRowKey(sess, table, sch, cells, r.key)
		if err := e.indexEntries(txn, sess, sch, defs, oldCells, r.key, false); err != nil {
			return nil, err
		}
		e.txn.Buffer(txn, transactions.Mutation{Key: bk, Value: encodeRow(cells)})
		if err := e.indexEntries(txn, sess, sch, defs, cells, r.key, true); err != nil {
			return nil, err
		}
		if len(s.ReturningList) > 0 {
			affected = append(affected, cells)
		}
		count++
	}
	result := &Result{Command: "UPDATE", RowsAffected: count}
	if len(s.ReturningList) > 0 {
		rr, err := projectReturning(sch, table, s.ReturningList, affected, params)
		if err != nil {
			return nil, err
		}
		rr.Command, rr.RowsAffected = "UPDATE", count
		result = rr
	}
	if err := e.commitTx(ctx, txn, owns); err != nil {
		return nil, err
	}
	return result, nil
}

// execDelete deletes rows matching WHERE. On main the key is removed; on a
// feature branch a tombstone is written to the branch prefix so the row is
// hidden without mutating the parent (by design).
// When a USING clause is present it delegates to execDeleteUsing (Task 4).
func (e *execImpl) execDelete(ctx context.Context, s *ast.DeleteStmt, sess *session.Session, params []Datum) (*Result, error) {
	if len(s.UsingClause) > 0 {
		return e.execDeleteUsing(ctx, s, sess, params)
	}

	table := s.Relation.RelName
	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	sc, err := e.scanTable(ctx, txn, sess, table, table)
	if err != nil {
		return nil, err
	}
	sch := sc.schema
	onBranch := sess.Branch() != "main"

	defs, err := e.loadIndexes(ctx, txn, sess, table)
	if err != nil {
		return nil, err
	}

	count := 0
	var affected [][]Datum
	for _, r := range sc.rows {
		ev := &evaluator{params: params, resolveCol: rowResolver(sch, table, r.cells), sess: sess}
		if s.WhereClause != nil {
			wv, err := ev.eval(s.WhereClause)
			if err != nil {
				return nil, err
			}
			if !asBool(wv) {
				continue
			}
		}
		// RLS visibility (USING) decides which rows DELETE may target; a hidden row
		// is silently skipped.
		if rlsApplies(sch, sess) {
			ok, err := e.rlsRowAllowed(sch, sess, "DELETE", params, rowResolver(sch, table, r.cells))
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		// FK parent-side: refuse to delete a row still referenced by a child
		// (ON DELETE RESTRICT, the default).
		if err := e.enforceFKParent(ctx, txn, sess, table, sch, r.cells); err != nil {
			return nil, err
		}
		bk := e.branchRowKey(sess, table, sch, r.cells, r.key)
		if onBranch {
			e.txn.Buffer(txn, transactions.Mutation{Key: bk, Value: storage.Tombstone()})
		} else {
			e.txn.Buffer(txn, transactions.Mutation{Key: bk, Delete: true})
		}
		if err := e.indexEntries(txn, sess, sch, defs, r.cells, r.key, false); err != nil {
			return nil, err
		}
		if len(s.ReturningList) > 0 {
			affected = append(affected, append([]Datum(nil), r.cells...))
		}
		count++
	}
	if len(s.ReturningList) > 0 {
		rr, err := projectReturning(sch, table, s.ReturningList, affected, params)
		if err != nil {
			return nil, err
		}
		rr.Command, rr.RowsAffected = "DELETE", count
		if err := e.commitTx(ctx, txn, owns); err != nil {
			return nil, err
		}
		return rr, nil
	}
	if err := e.commitTx(ctx, txn, owns); err != nil {
		return nil, err
	}
	return &Result{Command: "DELETE", RowsAffected: count}, nil
}

// evalIntLimit evaluates a LIMIT expression to a non-negative int (-1 = no limit).
func evalIntLimit(n ast.Node, params []Datum) (int, error) {
	ev := &evaluator{params: params}
	v, err := ev.eval(n)
	if err != nil {
		return -1, err
	}
	if v.null {
		return -1, nil
	}
	var i int
	if _, err := fmt.Sscanf(v.text, "%d", &i); err != nil {
		return -1, newExecError("22P02", "invalid LIMIT value %q", v.text)
	}
	return i, nil
}
