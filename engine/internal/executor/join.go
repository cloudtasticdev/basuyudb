package executor

import (
	"context"
	"strconv"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// binding associates a table alias + schema with one row's cells.
type binding struct {
	alias  string
	schema *tableSchema
	cells  []Datum
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

// rowCompositeColType builds a fieldColType resolver for a boundRow: given a
// column reference's fields, it returns the column's declared composite type name
// (when it has one), so (compositecol).field can decode by the column's type.
// Qualified references select the matching binding; unqualified references search
// all bindings.
func rowCompositeColType(row boundRow) func(fields []string) (string, bool) {
	return func(fields []string) (string, bool) {
		col := fields[len(fields)-1]
		if len(fields) > 1 {
			q := fields[0]
			for _, b := range row {
				if strings.EqualFold(b.alias, q) || strings.EqualFold(b.schema.Name, q) {
					if i := b.schema.colIndex(col); i >= 0 && b.schema.Cols[i].CompositeType != "" {
						return b.schema.Cols[i].CompositeType, true
					}
				}
			}
			return "", false
		}
		for _, b := range row {
			if i := b.schema.colIndex(col); i >= 0 && b.schema.Cols[i].CompositeType != "" {
				return b.schema.Cols[i].CompositeType, true
			}
		}
		return "", false
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
		// Common table expression reference: serve the materialized rows.
		if n.SchemaName == "" {
			if ent, ok := lookupCTE(ctx, n.RelName); ok {
				rows := make([]boundRow, 0, len(ent.rows))
				for _, cells := range ent.rows {
					rows = append(rows, boundRow{{alias: alias, schema: ent.schema, cells: cells}})
				}
				return rows, nil
			}
			// View reference: parse and execute the stored definition.
			if sql, ok, err := e.loadViewSQL(ctx, txn, sess, n.RelName); err != nil {
				return nil, err
			} else if ok {
				return e.materializeView(ctx, sess, alias, sql)
			}
		}
		// Catalog views for ORM introspection. Schema-qualified names use that
		// schema; unqualified pg_*/information_schema names resolve as PostgreSQL
		// does via the implicit pg_catalog (then information_schema) search path.
		schemasToTry := []string{n.SchemaName}
		if n.SchemaName == "" {
			schemasToTry = []string{"pg_catalog", "information_schema"}
		}
		for _, scn := range schemasToTry {
			if scn == "" {
				continue
			}
			if csch, crows, ok, err := e.catalogVirtualTable(ctx, txn, sess, scn, n.RelName); err != nil {
				return nil, err
			} else if ok {
				rows := make([]boundRow, 0, len(crows))
				for _, cells := range crows {
					rows = append(rows, boundRow{{alias: alias, schema: csch, cells: cells}})
				}
				return rows, nil
			}
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
		// LATERAL right side: the right relation is correlated and must be
		// evaluated once per left row with the left columns in scope.
		if isLateralFrom(n.Rarg) {
			return e.materializeLateralJoin(ctx, txn, sess, n, left, params)
		}
		right, err := e.materialize(ctx, txn, sess, n.Rarg, params)
		if err != nil {
			return nil, err
		}
		// Right-side schema template (for LEFT/FULL-JOIN NULL fill on empty/non-match).
		rightTmpl, err := e.bindingTemplate(ctx, txn, sess, n.Rarg)
		if err != nil {
			return nil, err
		}
		// Left-side schema template (for RIGHT/FULL-JOIN NULL fill of unmatched left).
		leftTmpl, err := e.bindingTemplate(ctx, txn, sess, n.Larg)
		if err != nil {
			return nil, err
		}
		return e.nestedLoopJoin(left, right, leftTmpl, rightTmpl, n, params)

	case *ast.SubLink:
		// Derived table: (SELECT ...) alias. Execute the subquery and bind its
		// result rows under the alias, like a CTE/view.
		sel, ok := n.SubSelect.(*ast.SelectStmt)
		if !ok {
			return nil, newExecError("0A000", "unsupported subquery FROM item")
		}
		alias := ""
		if n.Alias != nil {
			alias = n.Alias.AliasName
		}
		res, err := e.execSelect(ctx, sel, sess, params)
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

	case *ast.RangeSubselect:
		// A LATERAL subquery reaching materialize() directly (not as a JOIN's right
		// arm) has no left row in scope here; execute it uncorrelated. When it does
		// carry correlations, the JoinExpr path (materializeLateralJoin) handles it.
		sel, ok := n.Subquery.(*ast.SelectStmt)
		if !ok {
			return nil, newExecError("0A000", "unsupported LATERAL subquery")
		}
		alias := ""
		if n.Alias != nil {
			alias = n.Alias.AliasName
		}
		return e.materializeDerived(ctx, sess, alias, sel, params)

	case *ast.FuncCall:
		fname := strings.ToLower(strings.Join(n.FuncName, "."))
		if isSRFName(fname) {
			return e.materializeSRF(ctx, params, n, fname)
		}
		return e.materializeGenerateSeries(ctx, params, n)

	default:
		return nil, newExecError("0A000", "unsupported FROM item %T", node)
	}
}

// isLateralFrom reports whether a FROM node is a LATERAL item that must be
// evaluated correlated with preceding FROM rows.
func isLateralFrom(node ast.Node) bool {
	rs, ok := node.(*ast.RangeSubselect)
	return ok && rs.Lateral
}

// foldFromList collapses the comma-separated FROM list into a single left-deep
// join tree. Each comma is an implicit cross join, except when the right item is
// LATERAL — then it becomes an inner join whose Rarg is the LATERAL subselect, so
// materialize()'s LATERAL path evaluates it correlated against the items to its
// left (the same code path as explicit `JOIN LATERAL (...) ON true`). A nil/empty
// Quals on a CROSS / LATERAL join evaluates as TRUE.
func foldFromList(items []ast.Node) ast.Node {
	if len(items) == 0 {
		return nil
	}
	node := items[0]
	for _, next := range items[1:] {
		jt := ast.JOIN_CROSS
		if isLateralFrom(next) {
			// LATERAL is correlated; the LATERAL path keys off isLateralFrom(Rarg)
			// and applies joinMatch (nil Quals ⇒ TRUE), so an inner join with no ON
			// yields the correlated cross product.
			jt = ast.JOIN_INNER
		}
		node = &ast.JoinExpr{JoinType: jt, Larg: node, Rarg: next}
	}
	return node
}

// materializeDerived executes a subquery and binds its rows under alias (shared
// by the derived-table and uncorrelated-LATERAL paths).
func (e *execImpl) materializeDerived(ctx context.Context, sess *session.Session, alias string, sel *ast.SelectStmt, params []Datum) ([]boundRow, error) {
	res, err := e.execSelect(ctx, sel, sess, params)
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

// materializeLateralJoin evaluates a JOIN whose right side is a LATERAL subquery.
// For each left row it substitutes the left columns into the subquery (rendering
// it uncorrelated), executes it, and joins. LEFT JOIN LATERAL emits a NULL-filled
// right row when the subquery yields none (ON is required to be TRUE for LATERAL
// in practice; the ON predicate is still applied for INNER/LEFT correctness).
func (e *execImpl) materializeLateralJoin(ctx context.Context, txn *transactions.Txn, sess *session.Session, j *ast.JoinExpr, left []boundRow, params []Datum) ([]boundRow, error) {
	rs := j.Rarg.(*ast.RangeSubselect)
	sel, ok := rs.Subquery.(*ast.SelectStmt)
	if !ok {
		return nil, newExecError("0A000", "unsupported LATERAL subquery")
	}
	alias := ""
	if rs.Alias != nil {
		alias = rs.Alias.AliasName
	}
	leftJoin := j.JoinType == ast.JOIN_LEFT || j.JoinType == ast.JOIN_FULL

	// Right-side binding template (alias + column schema) for LEFT-JOIN NULL fill.
	// The subquery's output columns are stable across left rows, so derive the
	// shape once from the first left row's substitution (or an empty scope).
	var rightTmpl boundRow
	if leftJoin {
		var probeScope *outerScope
		if len(left) > 0 {
			probeScope = buildOuterScope(left[0])
		} else {
			probeScope = &outerScope{qualified: map[string]value{}, bare: map[string]value{}, ambiguous: map[string]bool{}}
		}
		if res, err := e.execSelect(ctx, substituteOuterSelect(sel, probeScope), sess, params); err == nil {
			sch := &tableSchema{Name: alias, PKIndex: -1}
			for _, col := range res.Columns {
				sch.Cols = append(sch.Cols, colMeta{Name: col.Name, TypeOID: col.TypeOID})
			}
			rightTmpl = boundRow{{alias: alias, schema: sch}}
		}
	}

	var out []boundRow
	for _, l := range left {
		subSel := substituteOuterSelect(sel, buildOuterScope(l))
		rightRows, err := e.materializeDerived(ctx, sess, alias, subSel, params)
		if err != nil {
			return nil, err
		}
		matched := false
		for _, r := range rightRows {
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
		if !matched && leftJoin && rightTmpl != nil {
			combined := append(append(boundRow{}, l...), nullFill(rightTmpl)...)
			out = append(out, combined)
		}
	}
	return out, nil
}

// materializeGenerateSeries evaluates generate_series(start, stop[, step]) as a
// table-valued function and returns one boundRow per value in the series.
func (e *execImpl) materializeGenerateSeries(ctx context.Context, params []Datum, f *ast.FuncCall) ([]boundRow, error) {
	fname := strings.ToLower(strings.Join(f.FuncName, "."))
	if fname != "generate_series" {
		return nil, newExecError("0A000", "set-returning function %q not supported in FROM", fname)
	}
	if len(f.Args) < 2 {
		return nil, newExecError("42883", "generate_series requires at least 2 arguments")
	}
	ev := &evaluator{params: params}
	startV, err := ev.eval(f.Args[0])
	if err != nil {
		return nil, err
	}
	stopV, err := ev.eval(f.Args[1])
	if err != nil {
		return nil, err
	}
	step := int64(1)
	if len(f.Args) >= 3 {
		stepV, err := ev.eval(f.Args[2])
		if err != nil {
			return nil, err
		}
		if !stepV.null {
			if sv, err2 := strconv.ParseInt(stepV.text, 10, 64); err2 == nil {
				step = sv
			}
		}
	}
	if step == 0 {
		return nil, newExecError("22023", "generate_series step cannot be zero")
	}
	start, _ := strconv.ParseInt(startV.text, 10, 64)
	stop, _ := strconv.ParseInt(stopV.text, 10, 64)

	alias := "generate_series"
	colName := "generate_series"
	if f.Alias != nil && f.Alias.AliasName != "" {
		alias = f.Alias.AliasName
		if len(f.Alias.ColNames) > 0 {
			colName = f.Alias.ColNames[0]
		}
	}
	schema := &tableSchema{Name: alias, PKIndex: -1, Cols: []colMeta{{Name: colName, TypeOID: OIDInt8}}}

	var rows []boundRow
	if step > 0 {
		for v := start; v <= stop; v += step {
			rows = append(rows, boundRow{{alias: alias, schema: schema,
				cells: []Datum{{Text: strconv.FormatInt(v, 10)}}}})
		}
	} else {
		for v := start; v >= stop; v += step {
			rows = append(rows, boundRow{{alias: alias, schema: schema,
				cells: []Datum{{Text: strconv.FormatInt(v, 10)}}}})
		}
	}
	return rows, nil
}

// materializeSRF evaluates a set-returning function (unnest / jsonb_array_elements
// / jsonb_array_elements_text) in the FROM clause, emitting one boundRow per
// element. The output column defaults to the function name and may be aliased.
func (e *execImpl) materializeSRF(ctx context.Context, params []Datum, f *ast.FuncCall, fname string) ([]boundRow, error) {
	ev := &evaluator{params: params}
	elems, _, err := ev.srfElements(f)
	if err != nil {
		return nil, err
	}
	alias := fname
	colName := fname
	if f.Alias != nil && f.Alias.AliasName != "" {
		alias = f.Alias.AliasName
		if len(f.Alias.ColNames) > 0 {
			colName = f.Alias.ColNames[0]
		} else {
			// PostgreSQL: a single set-returning function in FROM with a bare
			// alias `AS u` (no column list) names BOTH the relation and its
			// single output column `u`. Without this the column would keep the
			// function name and be invisible to ORDER BY/WHERE that reference u.
			colName = f.Alias.AliasName
		}
	}
	colOID := OIDText
	if len(elems) > 0 {
		colOID = elems[0].oid
	}
	schema := &tableSchema{Name: alias, PKIndex: -1, Cols: []colMeta{{Name: colName, TypeOID: colOID}}}
	rows := make([]boundRow, 0, len(elems))
	for _, el := range elems {
		rows = append(rows, boundRow{{alias: alias, schema: schema,
			cells: []Datum{{Null: el.null, Text: el.text}}}})
	}
	return rows, nil
}

// nestedLoopJoin combines left and right rows per the join type and ON/USING
// predicate. INNER, LEFT, RIGHT, FULL, and CROSS are all supported.
// leftTmpl is the left-side binding shape for RIGHT/FULL NULL-fill.
// rightTmpl is the right-side binding shape for LEFT/FULL NULL-fill.
func (e *execImpl) nestedLoopJoin(left, right []boundRow, leftTmpl, rightTmpl boundRow, j *ast.JoinExpr, params []Datum) ([]boundRow, error) {
	// For RIGHT JOIN, iterate right rows as the driver, null-fill missing left.
	if j.JoinType == ast.JOIN_RIGHT {
		var out []boundRow
		for _, r := range right {
			matched := false
			for _, l := range left {
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
			if !matched {
				combined := append(append(boundRow{}, nullFill(leftTmpl)...), r...)
				out = append(out, combined)
			}
		}
		return out, nil
	}

	// For FULL OUTER JOIN, track which right rows have been matched.
	var matchedRight map[int]bool
	if j.JoinType == ast.JOIN_FULL {
		matchedRight = make(map[int]bool, len(right))
	}

	var out []boundRow
	for _, l := range left {
		matched := false
		for ri, r := range right {
			combined := append(append(boundRow{}, l...), r...)
			ok, err := e.joinMatch(combined, j, params)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, combined)
				matched = true
				if matchedRight != nil {
					matchedRight[ri] = true
				}
			}
		}
		if !matched && (j.JoinType == ast.JOIN_LEFT || j.JoinType == ast.JOIN_FULL) {
			// Emit the left row with NULL-filled right columns.
			combined := append(append(boundRow{}, l...), nullFill(rightTmpl)...)
			out = append(out, combined)
		}
	}

	// FULL OUTER JOIN: emit unmatched right rows with NULL-filled left columns.
	if j.JoinType == ast.JOIN_FULL {
		for ri, r := range right {
			if !matchedRight[ri] {
				combined := append(append(boundRow{}, nullFill(leftTmpl)...), r...)
				out = append(out, combined)
			}
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
		// CTE / catalog tables resolve here too (not just real stored tables), so
		// a JOIN whose side is a CTE or pg_catalog view gets a binding template.
		if n.SchemaName == "" {
			if ent, ok := lookupCTE(ctx, n.RelName); ok {
				return boundRow{{alias: alias, schema: ent.schema}}, nil
			}
		}
		schemasToTry := []string{n.SchemaName}
		if n.SchemaName == "" {
			schemasToTry = []string{"pg_catalog", "information_schema"}
		}
		for _, scn := range schemasToTry {
			if scn == "" {
				continue
			}
			if csch, _, ok, err := e.catalogVirtualTable(ctx, txn, sess, scn, n.RelName); err != nil {
				return nil, err
			} else if ok {
				return boundRow{{alias: alias, schema: csch}}, nil
			}
		}
		sch, err := e.resolveSchema(ctx, txn, sess, n.RelName)
		if err != nil {
			return nil, err
		}
		return boundRow{{alias: alias, schema: sch}}, nil
	case *ast.SubLink:
		alias := ""
		if n.Alias != nil {
			alias = n.Alias.AliasName
		}
		sel, ok := n.SubSelect.(*ast.SelectStmt)
		if !ok {
			return nil, newExecError("0A000", "unsupported subquery FROM item")
		}
		res, err := e.execSelect(ctx, sel, sess, nil)
		if err != nil {
			return nil, err
		}
		sch := &tableSchema{Name: alias, PKIndex: -1}
		for _, col := range res.Columns {
			sch.Cols = append(sch.Cols, colMeta{Name: col.Name, TypeOID: col.TypeOID})
		}
		return boundRow{{alias: alias, schema: sch}}, nil
	case *ast.RangeSubselect:
		alias := ""
		if n.Alias != nil {
			alias = n.Alias.AliasName
		}
		sel, ok := n.Subquery.(*ast.SelectStmt)
		if !ok {
			return nil, newExecError("0A000", "unsupported LATERAL subquery")
		}
		res, err := e.execSelect(ctx, sel, sess, nil)
		if err != nil {
			return nil, err
		}
		sch := &tableSchema{Name: alias, PKIndex: -1}
		for _, col := range res.Columns {
			sch.Cols = append(sch.Cols, colMeta{Name: col.Name, TypeOID: col.TypeOID})
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
	case *ast.FuncCall:
		fname := strings.ToLower(strings.Join(n.FuncName, "."))
		alias := fname
		colName := fname
		if n.Alias != nil && n.Alias.AliasName != "" {
			alias = n.Alias.AliasName
			if len(n.Alias.ColNames) > 0 {
				colName = n.Alias.ColNames[0]
			}
		}
		schema := &tableSchema{Name: alias, PKIndex: -1, Cols: []colMeta{{Name: colName, TypeOID: OIDInt8}}}
		return boundRow{{alias: alias, schema: schema}}, nil
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
		// Multiple comma-separated FROM items form an implicit cross join (or, when
		// an item is LATERAL, a correlated join against the preceding items). Fold
		// the list into a left-deep join tree so all items are materialized and a
		// trailing `, LATERAL (...)` binds and evaluates correlated — matching the
		// explicit `CROSS JOIN` / `JOIN LATERAL ... ON true` forms.
		rows, err = e.materialize(ctx, txn, sess, foldFromList(s.FromClause), params)
		if err != nil {
			return nil, err
		}
	}

	// Row-Level Security: filter each base-table row through the combined USING
	// predicate (SELECT command) before WHERE. A row is dropped if any RLS-enabled
	// base table in its binding hides it. Default-deny applies when RLS is enabled
	// with no applicable policy. (RLS, this wave.)
	rows, err = e.applyRLSSelect(sess, rows, params)
	if err != nil {
		return nil, err
	}

	// WHERE — re-applied even on the index path (the scan is a pure accelerator).
	runSub := func(sel *ast.SelectStmt) (*Result, error) { return e.execSelect(ctx, sel, sess, params) }
	kept := make([]boundRow, 0, len(rows))
	for _, row := range rows {
		if s.WhereClause != nil {
			ev := &evaluator{params: params, resolveCol: combinedResolver(row), runSub: runSub, sess: sess}
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
	// Resolve SELECT-list aliases in ORDER BY (PostgreSQL allows ORDER BY
	// alias_name to refer to a SELECT-list output column name).
	if len(s.SortClause) > 0 && !orderedByIndex {
		sortClause := resolveOrderByAliases(s.SortClause, s.TargetList)
		if err := e.sortRows(kept, sortClause, params); err != nil {
			return nil, err
		}
	}

	// DISTINCT ON (exprs): keep only the first row of each group of adjacent rows
	// sharing the same DistinctOn key. Relies on a preceding ORDER BY (applied
	// above) having grouped rows by the key — matching PostgreSQL's requirement.
	// Done on boundRows (pre-projection) because the key expressions may not
	// appear in the SELECT list.
	if len(s.DistinctOn) > 0 {
		deduped, err := e.distinctOnRows(kept, s.DistinctOn, params)
		if err != nil {
			return nil, err
		}
		kept = deduped
	}

	// LIMIT is deferred past projection when DISTINCT (dedup first) or window
	// functions (computed over the full partition) are present, so it lands on
	// the final projected rows. DISTINCT ON already deduped boundRows above, so
	// its LIMIT can apply pre-projection like the normal path.
	deferLimit := (s.Distinct && len(s.DistinctOn) == 0) || hasWindowFunc(s.TargetList)
	if s.LimitCount != nil && !deferLimit {
		lim, err := evalIntLimit(s.LimitCount, params)
		if err != nil {
			return nil, err
		}
		if lim >= 0 && lim < len(kept) {
			kept = kept[:lim]
		}
	}

	res, err := e.projectRows(ctx, sess, s, kept, params)
	if err != nil {
		return nil, err
	}
	// Plain DISTINCT dedups the full projected tuple. DISTINCT ON was already
	// applied to the boundRows above and must NOT be re-deduped here (it keeps one
	// row per key, even if projected tuples coincide).
	if s.Distinct && len(s.DistinctOn) == 0 {
		res.Rows = dedupRows(res.Rows)
	}
	if s.LimitCount != nil && deferLimit {
		lim, err := evalIntLimit(s.LimitCount, params)
		if err != nil {
			return nil, err
		}
		if lim >= 0 && lim < len(res.Rows) {
			res.Rows = res.Rows[:lim]
		}
	}
	return res, nil
}

// distinctOnRows keeps the first row of each group of adjacent rows sharing the
// same DistinctOn key (PostgreSQL DISTINCT ON semantics). Rows are assumed
// pre-ordered by a compatible ORDER BY; we compare each row's key against the
// previous emitted key.
func (e *execImpl) distinctOnRows(rows []boundRow, onExprs []ast.Node, params []Datum) ([]boundRow, error) {
	out := make([]boundRow, 0, len(rows))
	var prevKey string
	have := false
	for _, row := range rows {
		ev := &evaluator{params: params, resolveCol: combinedResolver(row)}
		var b strings.Builder
		for _, ex := range onExprs {
			v, err := ev.eval(ex)
			if err != nil {
				return nil, err
			}
			if v.null {
				b.WriteByte(0)
				b.WriteByte('N')
			} else {
				b.WriteByte(1)
				b.WriteString(v.text)
			}
			b.WriteByte('|')
		}
		k := b.String()
		if have && k == prevKey {
			continue // same key as the previous emitted row: drop
		}
		out = append(out, row)
		prevKey = k
		have = true
	}
	return out, nil
}

// dedupRows removes duplicate rows by full-tuple equality, preserving the first
// occurrence order. NULLs are treated as equal to one another (SQL DISTINCT
// semantics, unlike UNIQUE which treats NULLs as distinct).
func dedupRows(rows [][]Datum) [][]Datum {
	seen := make(map[string]bool, len(rows))
	out := rows[:0:0]
	for _, r := range rows {
		var b strings.Builder
		for _, d := range r {
			if d.Null {
				b.WriteByte(0)
				b.WriteByte('N')
			} else {
				b.WriteByte(1)
				b.WriteString(d.Text)
			}
			b.WriteByte('|')
		}
		k := b.String()
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, r)
	}
	return out
}

// projectRows turns the final ordered/limited boundRows into a Result, expanding
// SELECT * or evaluating the target list.
func (e *execImpl) projectRows(ctx context.Context, sess *session.Session, s *ast.SelectStmt, rows []boundRow, params []Datum) (*Result, error) {
	starExpand := len(s.TargetList) == 1
	if starExpand {
		_, starExpand = s.TargetList[0].Val.(*ast.A_Star)
	}

	// Precompute window-function values (partition-aware) if any are present.
	windowVals, err := e.computeWindows(rows, s.TargetList, params)
	if err != nil {
		return nil, err
	}

	var cols []Column
	outRows := make([][]Datum, 0, len(rows))
	for ri, row := range rows {
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

		ev := &evaluator{params: params, resolveCol: combinedResolver(row), windowVals: windowVals, rowIdx: ri,
			sess: sess, lookupComposite: e.compositeResolverCtx(ctx, sess), fieldColType: rowCompositeColType(row)}

		// Single set-returning function in the SELECT list expands to one output
		// row per element of the produced set (common app pattern over a column).
		if srf, si := singleSRFTarget(s.TargetList); srf != nil {
			elems, _, err := ev.srfElements(srf)
			if err != nil {
				return nil, err
			}
			name := s.TargetList[si].Name
			if name == "" {
				name = defaultColName(srf, si)
			}
			colOID := OIDText
			if len(elems) > 0 {
				colOID = elems[0].oid
			}
			if cols == nil {
				cols = []Column{{Name: name, TypeOID: colOID}}
			}
			for _, el := range elems {
				outRows = append(outRows, []Datum{{Null: el.null, Text: el.text}})
			}
			continue
		}

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

// resolveOrderByAliases replaces ORDER BY expressions that match SELECT-list
// output aliases with the actual expression from the SELECT list. PostgreSQL
// allows ORDER BY alias_name to refer to a SELECT-list column by its AS name.
func resolveOrderByAliases(sortClause []*ast.SortBy, targetList []*ast.ResTarget) []*ast.SortBy {
	if len(sortClause) == 0 || len(targetList) == 0 {
		return sortClause
	}
	// Build alias → expression map from SELECT list.
	aliasMap := map[string]ast.Node{}
	for _, t := range targetList {
		if t.Name != "" {
			aliasMap[strings.ToLower(t.Name)] = t.Val
		}
	}
	if len(aliasMap) == 0 {
		return sortClause
	}
	// Replace any ORDER BY ColumnRef that matches a SELECT alias.
	out := make([]*ast.SortBy, len(sortClause))
	copy(out, sortClause)
	for i, sb := range out {
		cr, ok := sb.Node.(*ast.ColumnRef)
		if !ok || len(cr.Fields) == 0 {
			continue
		}
		col := strings.ToLower(cr.Fields[len(cr.Fields)-1])
		if expr, found := aliasMap[col]; found {
			// Shallow-copy the SortBy to avoid mutating the AST.
			cpy := *sb
			cpy.Node = expr
			out[i] = &cpy
		}
	}
	return out
}

// InferColumns is the public interface entry-point for emptyResultColumns — used
// by the wire layer's Describe path when execution fails (unsupported syntax).
func (e *execImpl) InferColumns(ctx context.Context, sess *session.Session, sel *ast.SelectStmt) []Column {
	return e.emptyResultColumns(ctx, sess, sel, false)
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
				} else {
					// Try catalog virtual tables (pg_catalog.pg_type, information_schema.*, etc.)
					// when the regular table lookup fails. This ensures the RowDescription for
					// catalog queries with 0-row results returns correct column OIDs instead of
					// defaulting everything to text — critical for Prisma/quaint which parses
					// column OIDs strictly.
					sn := rv.SchemaName
					tn := rv.RelName
					if sn == "" {
						// Try both pg_catalog and information_schema for unqualified catalog names.
						for _, candidate := range []string{"pg_catalog", "information_schema"} {
							if catalogSch, _, found, _ := e.catalogVirtualTable(ctx, txn, sess, candidate, tn); found {
								baseSch = catalogSch
								break
							}
						}
					} else {
						if catalogSch, _, found, _ := e.catalogVirtualTable(ctx, txn, sess, sn, tn); found {
							baseSch = catalogSch
						}
					}
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
