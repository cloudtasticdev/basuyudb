package executor

// advanced_dml.go implements:
//   - Multi-row INSERT VALUES (Task 1)
//   - INSERT ... SELECT (Task 2)
//   - UPDATE ... FROM (Task 3)
//   - DELETE ... USING (Task 4)
//   - CREATE TABLE AS SELECT (Task 7)
//
// WITH RECURSIVE (Task 5) is in cte_recursive.go.
// EXPLAIN (Task 6) is in explain.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// ── Task 1 & 2: extend execInsert ──────────────────────────────────────────
//
// execInsert (in dml.go) handles the single-row VALUES path.
// execInsertMultiRow and execInsertSelect are called when the multi-row or
// INSERT…SELECT variants are detected.

// execInsertMultiRow inserts multiple rows from InsertStmt.MultiRows.
// All rows share one transaction (autocommit or ambient explicit).
func (e *execImpl) execInsertMultiRow(ctx context.Context, s *ast.InsertStmt, sess *session.Session, params []Datum) (*Result, error) {
	totalAffected := 0
	var lastResult *Result

	// Re-use the single-row path by synthesising a one-row InsertStmt per row.
	for _, row := range s.MultiRows {
		// Build a SelectStmt whose TargetList has one ResTarget per value.
		targets := make([]*ast.ResTarget, len(row))
		for i, n := range row {
			targets[i] = &ast.ResTarget{Val: n}
		}
		single := &ast.InsertStmt{
			Relation:      s.Relation,
			Cols:          s.Cols,
			SelectStmt:    &ast.SelectStmt{TargetList: targets},
			OnConflict:    s.OnConflict,
			ReturningList: s.ReturningList,
		}
		res, err := e.execInsert(ctx, single, sess, params)
		if err != nil {
			return nil, err
		}
		totalAffected += res.RowsAffected
		lastResult = res
	}

	if lastResult != nil {
		lastResult.RowsAffected = totalAffected
		return lastResult, nil
	}
	return &Result{Command: "INSERT", RowsAffected: 0}, nil
}

// execInsertSelect handles INSERT ... SELECT where SelectStmt is a real query
// (has a FROM clause, set-operation, or WITH clause).
func (e *execImpl) execInsertSelect(ctx context.Context, s *ast.InsertStmt, sess *session.Session, params []Datum) (*Result, error) {
	sel, ok := s.SelectStmt.(*ast.SelectStmt)
	if !ok {
		return nil, newExecError("0A000", "INSERT ... SELECT: expected SelectStmt")
	}

	// Execute the SELECT to get the source rows.
	selResult, err := e.execSelect(ctx, sel, sess, params)
	if err != nil {
		return nil, err
	}

	// Insert each result row.
	totalAffected := 0
	for _, row := range selResult.Rows {
		// Build the VALUES list as A_Const nodes matching the result columns.
		targets := make([]*ast.ResTarget, len(row))
		for i, d := range row {
			if d.Null {
				targets[i] = &ast.ResTarget{Val: &ast.A_Const{Type: ast.ConstNull}}
			} else {
				targets[i] = &ast.ResTarget{Val: &ast.A_Const{Type: ast.ConstString, Val: d.Text}}
			}
		}

		// Determine the column list for this synthetic insert. If s.Cols is
		// empty, use the SELECT result column names (preserving order).
		cols := s.Cols
		if len(cols) == 0 && len(selResult.Columns) > 0 {
			cols = make([]*ast.ResTarget, len(selResult.Columns))
			for i, c := range selResult.Columns {
				cols[i] = &ast.ResTarget{Name: c.Name}
			}
		}

		single := &ast.InsertStmt{
			Relation:   s.Relation,
			Cols:       cols,
			SelectStmt: &ast.SelectStmt{TargetList: targets},
			OnConflict: s.OnConflict,
			// ReturningList intentionally omitted for intermediate rows
		}
		res, err := e.execInsert(ctx, single, sess, params)
		if err != nil {
			return nil, err
		}
		totalAffected += res.RowsAffected
	}

	return &Result{Command: "INSERT", RowsAffected: totalAffected}, nil
}

// ── Task 3: UPDATE … FROM ──────────────────────────────────────────────────

// execUpdateFrom handles UPDATE target SET … FROM source_list WHERE cond.
// For each target row it finds any matching FROM row, evaluates WHERE, and
// applies the SET expressions with access to columns from both tables.
func (e *execImpl) execUpdateFrom(ctx context.Context, s *ast.UpdateStmt, sess *session.Session, params []Datum) (*Result, error) {
	table := s.Relation.RelName

	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	// Scan target table.
	sc, err := e.scanTable(ctx, txn, sess, table, table)
	if err != nil {
		return nil, err
	}
	sch := sc.schema

	// Load index definitions for secondary-index maintenance.
	defs, err := e.loadIndexes(ctx, txn, sess, table)
	if err != nil {
		return nil, err
	}

	// Materialize FROM tables as combined rows.
	var fromRows []boundRow
	for _, f := range s.FromClause {
		fr, err := e.materialize(ctx, txn, sess, f, params)
		if err != nil {
			return nil, err
		}
		fromRows = append(fromRows, fr...)
	}

	count := 0
	var affected [][]Datum

	for _, r := range sc.rows {
		oldCells := append([]Datum(nil), r.cells...)
		cells := append([]Datum(nil), r.cells...)

		// Build a base binding for the target table row.
		targetBinding := binding{alias: table, schema: sch, cells: cells}

		matched := false
		var matchedFromRow boundRow

		if len(fromRows) == 0 {
			// No FROM rows: no match possible unless there are no FROM tables
			// (degenerate case). Treat as single pass with no FROM binding.
			matched = true
		} else {
			// Try every FROM row combination.
			for _, fromRow := range fromRows {
				combined := append(boundRow{targetBinding}, fromRow...)
				ev := &evaluator{params: params, resolveCol: combinedResolver(combined)}
				if s.WhereClause != nil {
					wv, err2 := ev.eval(s.WhereClause)
					if err2 != nil {
						return nil, err2
					}
					if !asBool(wv) {
						continue
					}
				}
				matched = true
				matchedFromRow = fromRow
				break // one match per target row
			}
		}

		if !matched {
			continue
		}

		// RLS visibility (USING) on the target row before applying changes.
		if rlsApplies(sch, sess) {
			ok, err := e.rlsRowAllowed(sch, sess, "UPDATE", params, rowResolver(sch, table, oldCells))
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}

		// Re-build the combined row for SET evaluation.
		combined := boundRow{targetBinding}
		combined = append(combined, matchedFromRow...)
		ev := &evaluator{params: params, resolveCol: combinedResolver(combined), sess: sess}

		// If WhereClause present and no FROM rows, re-evaluate with target only.
		if len(fromRows) == 0 && s.WhereClause != nil {
			wv, err2 := ev.eval(s.WhereClause)
			if err2 != nil {
				return nil, err2
			}
			if !asBool(wv) {
				continue
			}
		}

		// Apply SET expressions.
		for _, set := range s.TargetList {
			idx := sch.colIndex(set.Name)
			if idx < 0 {
				return nil, newExecError("42703", "column %q does not exist", set.Name)
			}
			v, err2 := ev.eval(set.Val)
			if err2 != nil {
				return nil, err2
			}
			cells[idx] = Datum{Null: v.null, Text: v.text}
		}

		// Recompute GENERATED STORED columns from the updated row.
		if err := e.applyGeneratedColumns(sch, table, cells, params); err != nil {
			return nil, err
		}

		// Constraints.
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
		if rlsApplies(sch, sess) {
			if err := e.rlsCheckNewRow(sch, sess, "UPDATE", params, rowResolver(sch, table, cells)); err != nil {
				return nil, err
			}
		}
		if parentRowRefChanged(sch, oldCells, cells) {
			if err := e.enforceFKParentUpdate(ctx, txn, sess, table, sch, oldCells, cells); err != nil {
				return nil, err
			}
		}

		// UNIQUE index enforcement on changed tuples.
		for _, d := range defs {
			if !d.Unique {
				continue
			}
			newTuple, ok2, err2 := encodeIndexTuple(sch, d.Columns, cells)
			if err2 != nil {
				return nil, err2
			}
			oldTuple, _, _ := encodeIndexTuple(sch, d.Columns, oldCells)
			if !ok2 || bytesEqual(newTuple, oldTuple) {
				continue
			}
			dup, err2 := e.indexHasOtherPK(ctx, txn, sess, d, newTuple, primaryKeyBytes(sch, cells, r.key))
			if err2 != nil {
				return nil, err2
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

// ── Task 4: DELETE … USING ─────────────────────────────────────────────────

// execDeleteUsing handles DELETE FROM target USING source_list WHERE cond.
// For each target row it checks whether any combination with a USING row
// satisfies WHERE; if so, the target row is deleted.
func (e *execImpl) execDeleteUsing(ctx context.Context, s *ast.DeleteStmt, sess *session.Session, params []Datum) (*Result, error) {
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

	// Materialize USING tables.
	var usingRows []boundRow
	for _, u := range s.UsingClause {
		ur, err := e.materialize(ctx, txn, sess, u, params)
		if err != nil {
			return nil, err
		}
		usingRows = append(usingRows, ur...)
	}

	count := 0
	var affected [][]Datum

	for _, r := range sc.rows {
		targetBinding := binding{alias: table, schema: sch, cells: r.cells}

		shouldDelete := false

		if len(usingRows) == 0 {
			// No USING clause rows to join: evaluate WHERE against target only.
			ev := &evaluator{params: params, resolveCol: rowResolver(sch, table, r.cells)}
			if s.WhereClause != nil {
				wv, err2 := ev.eval(s.WhereClause)
				if err2 != nil {
					return nil, err2
				}
				shouldDelete = asBool(wv)
			} else {
				shouldDelete = true
			}
		} else {
			// Try each USING row; delete if any match satisfies WHERE.
			for _, uRow := range usingRows {
				combined := append(boundRow{targetBinding}, uRow...)
				ev := &evaluator{params: params, resolveCol: combinedResolver(combined)}
				if s.WhereClause != nil {
					wv, err2 := ev.eval(s.WhereClause)
					if err2 != nil {
						return nil, err2
					}
					if !asBool(wv) {
						continue
					}
				}
				shouldDelete = true
				break
			}
		}

		if !shouldDelete {
			continue
		}

		// RLS visibility (USING) on the target row.
		if rlsApplies(sch, sess) {
			ok, err := e.rlsRowAllowed(sch, sess, "DELETE", params, rowResolver(sch, table, r.cells))
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}

		// FK parent-side check.
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

// ── Task 7: CREATE TABLE AS SELECT ─────────────────────────────────────────

// execCreateTableAs implements CREATE TABLE name AS SELECT ....
// It executes the SELECT, derives column definitions from the result columns,
// creates the table schema, then inserts all result rows.
func (e *execImpl) execCreateTableAs(ctx context.Context, s *ast.CreateStmt, sess *session.Session, params []Datum) (*Result, error) {
	table := s.Relation.RelName

	// Execute the SELECT first (outside the DDL transaction, using autocommit
	// or the ambient explicit txn).
	selResult, err := e.execSelect(ctx, s.AsSelect, sess, params)
	if err != nil {
		return nil, err
	}

	// Build the schema from SELECT result columns. No primary key is inferred.
	sch := tableSchema{Name: table, PKIndex: -1, Cols: make([]colMeta, len(selResult.Columns))}
	for i, c := range selResult.Columns {
		sch.Cols[i] = colMeta{
			Name:    strings.ToLower(c.Name),
			TypeOID: c.TypeOID,
		}
	}

	// Begin one transaction for the DDL + data load.
	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	// Check for existing table.
	if _, err := e.loadSchema(ctx, txn, sess, table); err == nil {
		if s.IfNotExists {
			return &Result{Command: "SELECT", RowsAffected: 0}, nil
		}
		return nil, newExecError("42P07", "relation %q already exists", table)
	}

	// Persist schema.
	rawSch, err := marshalJSON(&sch)
	if err != nil {
		return nil, err
	}
	key := e.store.Encoder().SchemaKey(sess.Namespace(), table)
	e.txn.Buffer(txn, transactions.Mutation{Key: key, Value: rawSch})

	// Insert all rows.
	enc := e.store.Encoder()
	for _, row := range selResult.Rows {
		// Pad / truncate to schema width.
		cells := make([]Datum, len(sch.Cols))
		for i := range cells {
			if i < len(row) {
				cells[i] = row[i]
			} else {
				cells[i] = Datum{Null: true}
			}
		}
		pk := []byte(fmt.Sprintf("%020d", txn.ReadTimestamp()))
		rowKey := enc.RowKey(sess.Namespace(), sess.Branch(), table, pk)
		e.txn.Buffer(txn, transactions.Mutation{Key: rowKey, Value: encodeRow(cells)})
	}

	if err := e.commitTx(ctx, txn, owns); err != nil {
		return nil, err
	}
	return &Result{Command: "SELECT", RowsAffected: len(selResult.Rows)}, nil
}

// marshalJSON is a thin wrapper used by execCreateTableAs.
func marshalJSON(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}
