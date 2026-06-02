package executor

import (
	"context"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// binding associates a table alias + schema with one row's cells.
type binding struct {
	alias string
	schema *tableSchema
	cells []Datum
}

// boundRow is a combined row across one or more joined tables.
type boundRow []binding

// combinedResolver resolves column references against a boundRow. A qualified
// reference (alias.col) selects the matching binding; an unqualified reference
// searches all bindings and errors if ambiguous.
func combinedResolver(row boundRow) func(fields []string) (value, error) {
	return func(fields []string) (value, error) {
		col := fields[len(fields)-1]
		if len(fields) > 1 {
			q := fields[0]
			for _, b := range row {
				if strings.EqualFold(b.alias, q) || strings.EqualFold(b.schema.Name, q) {
					idx := b.schema.colIndex(col)
					if idx < 0 {
						return value{}, newExecError("42703", "column %q does not exist in %q", col, q)
					}
					return cellValue(b, idx), nil
				}
			}
			return value{}, newExecError("42P01", "missing FROM-clause entry for table %q", q)
		}
		// Unqualified: search all bindings.
		var found *binding
		var foundIdx int
		for i := range row {
			if idx := row[i].schema.colIndex(col); idx >= 0 {
				if found != nil {
					return value{}, newExecError("42702", "column reference %q is ambiguous", col)
				}
				found = &row[i]
				foundIdx = idx
			}
		}
		if found == nil {
			return value{}, newExecError("42703", "column %q does not exist", col)
		}
		return cellValue(*found, foundIdx), nil
	}
}

func cellValue(b binding, idx int) value {
	c := b.cells[idx]
	return value{null: c.Null, text: c.Text, oid: b.schema.Cols[idx].TypeOID}
}

// materialize turns a FROM item (a table or a JOIN tree) into combined rows.
func (e *execImpl) materialize(ctx context.Context, txn *transactions.Txn, sess *session.Session, node ast.Node, params []Datum) ([]boundRow, error) {
	switch n := node.(type) {
	case *ast.RangeVar:
		alias := n.RelName
		if n.Alias != nil && n.Alias.AliasName != "" {
			alias = n.Alias.AliasName
		}
		sc, err := e.scanTable(ctx, txn, sess, n.RelName, alias)
		if err != nil {
			return nil, err
		}
		rows := make([]boundRow, 0, len(sc.rows))
		for _, r := range sc.rows {
			rows = append(rows, boundRow{{alias: sc.alias, schema: sc.schema, cells: r.cells}})
		}
		return rows, nil

	case *ast.JoinExpr:
		left, err := e.materialize(ctx, txn, sess, n.Larg, params)
		if err != nil {
			return nil, err
		}
		right, err := e.materialize(ctx, txn, sess, n.Rarg, params)
		if err != nil {
			return nil, err
		}
		// Right-side schema template (for LEFT-JOIN NULL fill on empty/non-match).
		rightTmpl, err := e.bindingTemplate(ctx, txn, sess, n.Rarg)
		if err != nil {
			return nil, err
		}
		return e.nestedLoopJoin(left, right, rightTmpl, n, params)

	default:
		return nil, newExecError("0A000", "unsupported FROM item %T", node)
	}
}

// nestedLoopJoin combines left and right rows per the join type and ON/USING
// predicate. INNER and LEFT are supported in milestone-4 (covers the OTel demo).
// rightTmpl is the right-side binding shape used to NULL-fill LEFT-JOIN misses.
func (e *execImpl) nestedLoopJoin(left, right []boundRow, rightTmpl boundRow, j *ast.JoinExpr, params []Datum) ([]boundRow, error) {
	var out []boundRow
	for _, l := range left {
		matched := false
		for _, r := range right {
			combined := append(append(boundRow{}, l...), r...)
			ok, err := e.joinMatch(combined, j, params)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, combined)
				matched = true
			}
		}
		if !matched && j.JoinType == ast.JOIN_LEFT {
			// Emit the left row with NULL-filled right columns.
			combined := append(append(boundRow{}, l...), nullFill(rightTmpl)...)
			out = append(out, combined)
		}
	}
	return out, nil
}

// bindingTemplate returns the binding shape (alias + schema, nil cells) for a
// FROM node, independent of how many rows it has. Used for LEFT-JOIN NULL fill.
func (e *execImpl) bindingTemplate(ctx context.Context, txn *transactions.Txn, sess *session.Session, node ast.Node) (boundRow, error) {
	switch n := node.(type) {
	case *ast.RangeVar:
		alias := n.RelName
		if n.Alias != nil && n.Alias.AliasName != "" {
			alias = n.Alias.AliasName
		}
		sch, err := e.resolveSchema(ctx, txn, sess, n.RelName)
		if err != nil {
			return nil, err
		}
		return boundRow{{alias: alias, schema: sch}}, nil
	case *ast.JoinExpr:
		l, err := e.bindingTemplate(ctx, txn, sess, n.Larg)
		if err != nil {
			return nil, err
		}
		r, err := e.bindingTemplate(ctx, txn, sess, n.Rarg)
		if err != nil {
			return nil, err
		}
		return append(l, r...), nil
	default:
		return nil, newExecError("0A000", "unsupported FROM item %T", node)
	}
}

// nullFill produces NULL-celled bindings matching a binding template.
func nullFill(tmpl boundRow) boundRow {
	out := make(boundRow, len(tmpl))
	for i, b := range tmpl {
		cells := make([]Datum, len(b.schema.Cols))
		for j := range cells {
			cells[j] = Datum{Null: true}
		}
		out[i] = binding{alias: b.alias, schema: b.schema, cells: cells}
	}
	return out
}

// joinMatch evaluates the ON predicate (or USING equality) for a combined row.
func (e *execImpl) joinMatch(row boundRow, j *ast.JoinExpr, params []Datum) (bool, error) {
	if j.JoinType == ast.JOIN_CROSS {
		return true, nil
	}
	if len(j.UsingCols) > 0 {
		res := combinedResolver(row)
		for _, col := range j.UsingCols {
			// USING(col): both sides' col must be equal. Resolve unqualified.
			lv, err := res([]string{col})
			if err != nil {
				return false, err
			}
			// Resolve the same column on each side explicitly is complex; for
			// milestone-4 USING requires the column present once per side, so a
			// direct equality of the (ambiguous-allowed) lookups is sufficient
			// because both sides share the name. We compare the two bindings.
			_ = lv
		}
		return usingEqual(row, j.UsingCols)
	}
	if j.Quals == nil {
		return true, nil
	}
	ev := &evaluator{params: params, resolveCol: combinedResolver(row)}
	v, err := ev.eval(j.Quals)
	if err != nil {
		return false, err
	}
	return asBool(v), nil
}

// usingEqual checks equality of each USING column across the two halves of a
// joined row (left bindings precede right bindings).
func usingEqual(row boundRow, cols []string) (bool, error) {
	for _, col := range cols {
		var vals []value
		for _, b := range row {
			if idx := b.schema.colIndex(col); idx >= 0 {
				vals = append(vals, cellValue(b, idx))
			}
		}
		if len(vals) < 2 {
			return false, newExecError("42703", "USING column %q not present on both sides", col)
		}
		if vals[0].null || vals[1].null || vals[0].text != vals[1].text {
			return false, nil
		}
	}
	return true, nil
}

// execSelectFrom scans/joins the FROM clause, then applies WHERE, ORDER BY, and
// LIMIT, and projects the target list. A single-table SELECT may be served by an
// ordered secondary index (planIndexScan); otherwise it falls back to a full
// scan/JOIN. ORDER BY is applied in memory unless the index already produced the
// requested order. (Single-table and JOIN share this path.)
func (e *execImpl) execSelectFrom(ctx context.Context, s *ast.SelectStmt, sess *session.Session, params []Datum) (*Result, error) {
	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	scan, err := e.planIndexScan(ctx, txn, sess, s, params)
	if err != nil {
		return nil, err
	}
	var rows []boundRow
	orderedByIndex := false
	if scan != nil {
		rows, orderedByIndex = scan.rows, scan.ordered
	} else {
		rows, err = e.materialize(ctx, txn, sess, s.FromClause[0], params)
		if err != nil {
			return nil, err
		}
	}

	// WHERE — re-applied even on the index path (the scan is a pure accelerator).
	runSub := func(sel *ast.SelectStmt) (*Result, error) { return e.execSelect(ctx, sel, sess, params) }
	kept := make([]boundRow, 0, len(rows))
	for _, row := range rows {
		if s.WhereClause != nil {
			ev := &evaluator{params: params, resolveCol: combinedResolver(row), runSub: runSub}
			wv, err := ev.eval(s.WhereClause)
			if err != nil {
				return nil, err
			}
			if !asBool(wv) {
				continue
			}
		}
		kept = append(kept, row)
	}

	// Aggregation (GROUP BY / aggregate functions) takes its own path: group,
	// apply HAVING, project per group, then ORDER BY + LIMIT over the groups.
	if needsAggregation(s) {
		return e.execAggregate(ctx, sess, s, kept, params)
	}

	// ORDER BY — sort in memory unless the index already produced this order.
	if len(s.SortClause) > 0 && !orderedByIndex {
		if err := e.sortRows(kept, s.SortClause, params); err != nil {
			return nil, err
		}
	}

	// LIMIT.
	if s.LimitCount != nil {
		lim, err := evalIntLimit(s.LimitCount, params)
		if err != nil {
			return nil, err
		}
		if lim >= 0 && lim < len(kept) {
			kept = kept[:lim]
		}
	}

	return e.projectRows(ctx, sess, s, kept, params)
}

// projectRows turns the final ordered/limited boundRows into a Result, expanding
// SELECT * or evaluating the target list.
func (e *execImpl) projectRows(ctx context.Context, sess *session.Session, s *ast.SelectStmt, rows []boundRow, params []Datum) (*Result, error) {
	starExpand := len(s.TargetList) == 1
	if starExpand {
		_, starExpand = s.TargetList[0].Val.(*ast.A_Star)
	}

	var cols []Column
	outRows := make([][]Datum, 0, len(rows))
	for _, row := range rows {
		if starExpand {
			if cols == nil {
				for _, b := range row {
					for _, c := range b.schema.Cols {
						cols = append(cols, Column{Name: c.Name, TypeOID: c.TypeOID})
					}
				}
			}
			var out []Datum
			for _, b := range row {
				out = append(out, b.cells...)
			}
			outRows = append(outRows, out)
			continue
		}

		ev := &evaluator{params: params, resolveCol: combinedResolver(row)}
		out := make([]Datum, len(s.TargetList))
		rowCols := make([]Column, len(s.TargetList))
		for i, t := range s.TargetList {
			v, err := ev.eval(t.Val)
			if err != nil {
				return nil, err
			}
			out[i] = Datum{Null: v.null, Text: v.text}
			name := t.Name
			if name == "" {
				name = defaultColName(t.Val, i)
			}
			rowCols[i] = Column{Name: name, TypeOID: v.oid}
		}
		if cols == nil {
			cols = rowCols
		}
		outRows = append(outRows, out)
	}

	if cols == nil {
		cols = e.emptyResultColumns(ctx, sess, s, starExpand)
	}
	return &Result{Columns: cols, Rows: outRows, Command: "SELECT"}, nil
}

// emptyResultColumns derives a column descriptor when the result set is empty
// (so clients receive a RowDescription). Best-effort for milestone-4.
func (e *execImpl) emptyResultColumns(ctx context.Context, sess *session.Session, s *ast.SelectStmt, star bool) []Column {
	// Resolve the single base table's schema (if the FROM is one table) so that
	// even an empty result set carries correct column types — drivers/ORMs that
	// describe a prepared statement need this (a 0-row probe must still report
	// `int`, not text).
	var baseSch *tableSchema
	if len(s.FromClause) == 1 {
		if rv, ok := s.FromClause[0].(*ast.RangeVar); ok {
			if txn, owns, err := e.beginTx(ctx, sess.Auth); err == nil {
				if sch, err := e.resolveSchema(ctx, txn, sess, rv.RelName); err == nil {
					baseSch = sch
				}
				e.rollbackTx(ctx, txn, owns)
			}
		}
	}

	if !star {
		cols := make([]Column, len(s.TargetList))
		for i, t := range s.TargetList {
			name := t.Name
			if name == "" {
				name = defaultColName(t.Val, i)
			}
			oid := uint32(OIDText)
			// A bare column reference inherits its declared type from the schema.
			if cr, ok := t.Val.(*ast.ColumnRef); ok && baseSch != nil && len(cr.Fields) > 0 {
				if ci := baseSch.colIndex(cr.Fields[len(cr.Fields)-1]); ci >= 0 {
					oid = baseSch.Cols[ci].TypeOID
				}
			}
			cols[i] = Column{Name: name, TypeOID: oid}
		}
		return cols
	}
	// SELECT * with no rows: expand the base table's columns.
	if baseSch != nil {
		cols := make([]Column, len(baseSch.Cols))
		for i, c := range baseSch.Cols {
			cols[i] = Column{Name: c.Name, TypeOID: c.TypeOID}
		}
		return cols
	}
	return []Column{}
}
