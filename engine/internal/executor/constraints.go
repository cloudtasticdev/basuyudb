package executor

import (
	"context"
	"errors"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/parser"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// deparseExpr renders a parsed expression back to SQL text, used to persist a
// CHECK constraint so it can be re-parsed and evaluated at write time. Binary
// operators are fully parenthesized so the round-trip is precedence-safe.
func deparseExpr(n ast.Node) (string, error) {
	switch e := n.(type) {
	case *ast.A_Const:
		switch e.Type {
		case ast.ConstNull:
			return "NULL", nil
		case ast.ConstString:
			return "'" + strings.ReplaceAll(e.Val, "'", "''") + "'", nil
		case ast.ConstBool:
			if e.Val == "true" {
				return "TRUE", nil
			}
			return "FALSE", nil
		default: // int / float
			return e.Val, nil
		}
	case *ast.ColumnRef:
		return strings.Join(e.Fields, "."), nil
	case *ast.A_Expr:
		r, err := deparseExpr(e.Rexpr)
		if err != nil {
			return "", err
		}
		if e.Lexpr == nil { // unary
			return "(" + e.Name + r + ")", nil
		}
		l, err := deparseExpr(e.Lexpr)
		if err != nil {
			return "", err
		}
		if e.Kind == ast.AEXPR_IN {
			return "(" + l + " IN " + r + ")", nil
		}
		return "(" + l + " " + e.Name + " " + r + ")", nil
	case *ast.BoolExpr:
		parts := make([]string, 0, len(e.Args))
		for _, a := range e.Args {
			s, err := deparseExpr(a)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		switch e.Op {
		case ast.NOT_EXPR:
			return "(NOT " + parts[0] + ")", nil
		case ast.AND_EXPR:
			return "(" + strings.Join(parts, " AND ") + ")", nil
		default:
			return "(" + strings.Join(parts, " OR ") + ")", nil
		}
	case *ast.NullTest:
		a, err := deparseExpr(e.Arg)
		if err != nil {
			return "", err
		}
		if e.TestNull {
			return "(" + a + " IS NULL)", nil
		}
		return "(" + a + " IS NOT NULL)", nil
	case *ast.List:
		parts := make([]string, 0, len(e.Items))
		for _, it := range e.Items {
			s, err := deparseExpr(it)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return "(" + strings.Join(parts, ", ") + ")", nil
	case *ast.FuncCall:
		var inner string
		if e.AggStar {
			inner = "*"
		} else {
			args := make([]string, 0, len(e.Args))
			for _, a := range e.Args {
				s, err := deparseExpr(a)
				if err != nil {
					return "", err
				}
				args = append(args, s)
			}
			inner = strings.Join(args, ", ")
		}
		out := strings.Join(e.FuncName, ".") + "(" + inner + ")"
		if e.Over != nil {
			w, err := deparseWindow(e.Over)
			if err != nil {
				return "", err
			}
			out += " OVER (" + w + ")"
		}
		return out, nil
	case *ast.TypeCast:
		a, err := deparseExpr(e.Arg)
		if err != nil {
			return "", err
		}
		return "(" + a + "::" + e.TypeName + ")", nil
	case *ast.A_Star:
		return "*", nil
	case *ast.CaseExpr:
		var b strings.Builder
		b.WriteString("CASE")
		if e.Arg != nil {
			a, err := deparseExpr(e.Arg)
			if err != nil {
				return "", err
			}
			b.WriteString(" " + a)
		}
		for _, w := range e.Whens {
			c, err := deparseExpr(w.Cond)
			if err != nil {
				return "", err
			}
			r, err := deparseExpr(w.Result)
			if err != nil {
				return "", err
			}
			b.WriteString(" WHEN " + c + " THEN " + r)
		}
		if e.Else != nil {
			el, err := deparseExpr(e.Else)
			if err != nil {
				return "", err
			}
			b.WriteString(" ELSE " + el)
		}
		b.WriteString(" END")
		return b.String(), nil
	case *ast.SubLink:
		sub, ok := e.SubSelect.(*ast.SelectStmt)
		if !ok {
			return "", newExecError("0A000", "unsupported subquery in view")
		}
		s, err := deparseSelect(sub)
		if err != nil {
			return "", err
		}
		return "(" + s + ")", nil
	default:
		return "", newExecError("0A000", "unsupported expression %T in view/constraint", n)
	}
}

// deparseWindow renders an OVER (...) specification.
func deparseWindow(w *ast.WindowDef) (string, error) {
	var parts []string
	if len(w.PartitionBy) > 0 {
		cols := make([]string, 0, len(w.PartitionBy))
		for _, p := range w.PartitionBy {
			s, err := deparseExpr(p)
			if err != nil {
				return "", err
			}
			cols = append(cols, s)
		}
		parts = append(parts, "PARTITION BY "+strings.Join(cols, ", "))
	}
	if len(w.OrderBy) > 0 {
		ob, err := deparseSortList(w.OrderBy)
		if err != nil {
			return "", err
		}
		parts = append(parts, "ORDER BY "+ob)
	}
	return strings.Join(parts, " "), nil
}

// parseStoredExpr re-parses a persisted expression by wrapping it as a SELECT
// target and extracting the expression node.
func parseStoredExpr(text string) (ast.Node, error) {
	stmt, err := parser.Parse("SELECT " + text)
	if err != nil {
		return nil, newExecError("XX000", "re-parse CHECK %q: %v", text, err)
	}
	sel, ok := stmt.(*ast.SelectStmt)
	if !ok || len(sel.TargetList) == 0 {
		return nil, newExecError("XX000", "re-parse CHECK %q failed", text)
	}
	return sel.TargetList[0].Val, nil
}

// parentRowRefChanged reports whether an UPDATE changed any column value of a
// row, so the ON UPDATE referential-action pass should run. The pass itself
// matches child rows on the precise old referenced tuple, so over-firing here is
// safe — it just costs a child scan; we skip it entirely when nothing changed.
// Modeling exactly which columns a child FK references would need a cross-table
// scan, so a value-level diff is the conservative, correct trigger.
func parentRowRefChanged(sch *tableSchema, oldCells, newCells []Datum) bool {
	for i := range oldCells {
		if i >= len(newCells) {
			break
		}
		if oldCells[i].Null != newCells[i].Null || oldCells[i].Text != newCells[i].Text {
			return true
		}
	}
	return false
}

// recordForeignKey writes a (possibly composite) FOREIGN KEY constraint onto a
// schema. The constraint is anchored on its first local column: FKTable/FKColumn
// carry the first pair (back-compat with single-column enforcement) and FKCols
// carries every (local, referenced) pair when the key spans more than one column.
// Referential actions are recorded on the anchor column. Shared by CREATE TABLE
// (table-level constraint) and ALTER TABLE ... ADD CONSTRAINT.
func recordForeignKey(sch *tableSchema, table string, c *ast.AlterTableConstraint) error {
	if len(c.Columns) == 0 {
		return newExecError("42601", "FOREIGN KEY requires at least one column")
	}
	if len(c.RefColumns) != 0 && len(c.RefColumns) != len(c.Columns) {
		return newExecError("42830",
			"number of referencing and referenced columns for foreign key disagree")
	}
	anchor := sch.colIndex(c.Columns[0])
	if anchor < 0 {
		return newExecError("42703", "column %q of relation %q does not exist", c.Columns[0], table)
	}
	pairs := make([]fkColPair, len(c.Columns))
	for i, local := range c.Columns {
		if sch.colIndex(local) < 0 {
			return newExecError("42703", "column %q of relation %q does not exist", local, table)
		}
		ref := ""
		if i < len(c.RefColumns) {
			ref = c.RefColumns[i]
		}
		pairs[i] = fkColPair{Local: local, Ref: ref}
	}
	sch.Cols[anchor].FKTable = c.RefTable
	sch.Cols[anchor].FKColumn = pairs[0].Ref
	sch.Cols[anchor].FKOnDelete = c.OnDelete
	sch.Cols[anchor].FKOnUpdate = c.OnUpdate
	name := c.Name
	if name == "" {
		name = table + "_" + strings.Join(c.Columns, "_") + "_fkey"
	}
	sch.Cols[anchor].FKName = name
	// Table-level / ADD CONSTRAINT foreign keys carry deferrability only if the
	// grammar captured it; today it does not, so these default to false
	// (non-deferrable, always checked immediately).
	sch.Cols[anchor].FKDeferrable = c.Deferrable
	sch.Cols[anchor].FKInitiallyDeferred = c.InitiallyDeferred
	if len(pairs) > 1 {
		sch.Cols[anchor].FKCols = pairs
	} else {
		sch.Cols[anchor].FKCols = nil
	}
	return nil
}

// recordCheck attaches a deparsed CHECK predicate to a schema (on the first
// column, AND-combined with any existing check there). enforceChecks evaluates
// each non-empty per-column check against the full row, so placement is
// immaterial. Shared by CREATE TABLE and ALTER TABLE ... ADD CONSTRAINT.
func recordCheck(sch *tableSchema, expr ast.Node) error {
	txt, err := deparseExpr(expr)
	if err != nil {
		return err
	}
	if len(sch.Cols) == 0 {
		return newExecError("42P10", "cannot add CHECK to a table with no columns")
	}
	if sch.Cols[0].Check == "" {
		sch.Cols[0].Check = txt
	} else {
		sch.Cols[0].Check = "(" + sch.Cols[0].Check + " AND " + txt + ")"
	}
	return nil
}

// enforceChecks evaluates every column CHECK constraint against a candidate row.
// PostgreSQL semantics: a CHECK fails only when it evaluates to FALSE; NULL
// (unknown) passes.
func (e *execImpl) enforceChecks(sch *tableSchema, table string, cells []Datum, params []Datum) error {
	for _, c := range sch.Cols {
		if c.Check == "" {
			continue
		}
		node, err := parseStoredExpr(c.Check)
		if err != nil {
			return err
		}
		ev := &evaluator{params: params, resolveCol: rowResolver(sch, table, cells)}
		v, err := ev.eval(node)
		if err != nil {
			return err
		}
		if !v.null && !asBool(v) {
			return newExecError("23514", "new row for relation %q violates check constraint on %q", table, c.Name)
		}
	}
	return nil
}

// fkConstraint describes one foreign key anchored on a child column: the parent
// table and the ordered (local, referenced) column pairs. A single-column FK has
// one pair; a composite FK has several.
type fkConstraint struct {
	anchor int // index of the anchor (first local) column in the child schema
	table  string
	pairs  []fkColPair
	// name / deferrable / initiallyDeferred describe the constraint for
	// DEFERRABLE checking (SET CONSTRAINTS, deferred enforcement at COMMIT).
	name              string
	deferrable        bool
	initiallyDeferred bool
}

// childForeignKeys returns the foreign keys declared on a child schema, expanding
// the anchor column's FKCols (composite) or its FKTable/FKColumn (single-column).
func childForeignKeys(sch *tableSchema) []fkConstraint {
	var out []fkConstraint
	for i, c := range sch.Cols {
		if c.FKTable == "" {
			continue
		}
		pairs := c.FKCols
		if len(pairs) == 0 {
			pairs = []fkColPair{{Local: c.Name, Ref: c.FKColumn}}
		}
		name := c.FKName
		if name == "" {
			name = sch.Name + "_" + c.Name + "_fkey"
		}
		out = append(out, fkConstraint{
			anchor:            i,
			table:             c.FKTable,
			pairs:             pairs,
			name:              name,
			deferrable:        c.FKDeferrable,
			initiallyDeferred: c.FKInitiallyDeferred,
		})
	}
	return out
}

// enforceFKChild verifies that every foreign key on a candidate row references an
// existing parent row (referential integrity, child side). A composite FK matches
// on the full tuple of FK columns; a row with any NULL FK column is skipped (PG's
// MATCH SIMPLE: a partially-NULL composite key is not enforced).
func (e *execImpl) enforceFKChild(ctx context.Context, txn *transactions.Txn, sess *session.Session, sch *tableSchema, cells []Datum) error {
	for _, fk := range childForeignKeys(sch) {
		localVals := make([]string, len(fk.pairs))
		refCols := make([]string, len(fk.pairs))
		anyNull := false
		for k, p := range fk.pairs {
			ci := sch.colIndex(p.Local)
			if ci < 0 || cells[ci].Null {
				anyNull = true
				break
			}
			localVals[k] = cells[ci].Text
			refCols[k] = p.Ref
		}
		if anyNull {
			continue // MATCH SIMPLE: skip when any FK column is NULL
		}
		// DEFERRABLE constraint deferred in this explicit transaction: record the
		// existence check to re-run at COMMIT (or at SET CONSTRAINTS IMMEDIATE)
		// instead of raising now.
		if e.effectiveDeferral(ctx, fk) {
			e.deferred.stateFor(txn).record(pendingFKCheck{
				kind:       pendingFKChild,
				constraint: fk.name,
				childTable: sch.Name,
				parentTab:  fk.table,
				refCols:    append([]string(nil), refCols...),
				vals:       append([]string(nil), localVals...),
			})
			continue
		}
		ok, err := e.parentTupleExists(ctx, txn, sess, fk.table, refCols, localVals)
		if err != nil {
			return err
		}
		if !ok {
			cols := make([]string, len(fk.pairs))
			for k, p := range fk.pairs {
				cols[k] = p.Local
			}
			return newExecError("23503",
				"insert or update on table violates foreign key constraint: key (%s)=(%s) is not present in table %q",
				strings.Join(cols, ", "), strings.Join(localVals, ", "), fk.table)
		}
	}
	return nil
}

// parentTupleExists reports whether parentTable has a row whose referenced
// columns equal vals. A single-column reference to the parent PK is a direct key
// probe; everything else (composite, or non-PK single column) scans the parent.
func (e *execImpl) parentTupleExists(ctx context.Context, txn *transactions.Txn, sess *session.Session, parentTable string, refCols, vals []string) (bool, error) {
	if len(refCols) == 1 {
		return e.parentRowExists(ctx, txn, sess, parentTable, refCols[0], vals[0])
	}
	psch, err := e.loadSchema(ctx, txn, sess, parentTable)
	if err != nil {
		return false, err
	}
	idxs := make([]int, len(refCols))
	for k, rc := range refCols {
		ci := psch.colIndex(rc)
		if ci < 0 {
			return false, newExecError("42703", "referenced column %q does not exist in %q", rc, parentTable)
		}
		idxs[k] = ci
	}
	sc, err := e.scanTable(ctx, txn, sess, parentTable, parentTable)
	if err != nil {
		return false, err
	}
	for _, r := range sc.rows {
		match := true
		for k, ci := range idxs {
			if r.cells[ci].Null || r.cells[ci].Text != vals[k] {
				match = false
				break
			}
		}
		if match {
			return true, nil
		}
	}
	return false, nil
}

// parentRowExists reports whether parentTable has a row whose referenced column
// (parentCol, or its primary key when empty) equals val. A PK reference is a
// direct key probe; a non-PK reference scans the parent.
func (e *execImpl) parentRowExists(ctx context.Context, txn *transactions.Txn, sess *session.Session, parentTable, parentCol, val string) (bool, error) {
	psch, err := e.loadSchema(ctx, txn, sess, parentTable)
	if err != nil {
		return false, err
	}
	// Reference to the parent primary key: direct row-key probe.
	if parentCol == "" || (psch.PKIndex >= 0 && strings.EqualFold(parentCol, psch.Cols[psch.PKIndex].Name)) {
		key := e.store.Encoder().RowKey(sess.Namespace(), sess.Branch(), parentTable, []byte(val))
		if _, err := e.txn.Get(ctx, txn, key); err != nil {
			if errors.Is(err, storage.ErrKeyNotFound) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	// Reference to a non-PK column: scan parent rows for a match.
	ci := psch.colIndex(parentCol)
	if ci < 0 {
		return false, newExecError("42703", "referenced column %q does not exist in %q", parentCol, parentTable)
	}
	sc, err := e.scanTable(ctx, txn, sess, parentTable, parentTable)
	if err != nil {
		return false, err
	}
	for _, r := range sc.rows {
		if !r.cells[ci].Null && r.cells[ci].Text == val {
			return true, nil
		}
	}
	return false, nil
}

// enforceFKParent applies the referential action of every child FK that
// references a parent row being deleted (ON DELETE) — RESTRICT/NO ACTION block,
// CASCADE deletes the children, SET NULL nulls the child FK columns. It is the
// ON DELETE entry point (depth 0 of the cascade). psch/pcells describe the parent
// row being deleted so composite FKs can match on the full referenced tuple.
func (e *execImpl) enforceFKParent(ctx context.Context, txn *transactions.Txn, sess *session.Session, parentTable string, psch *tableSchema, pcells []Datum) error {
	return e.applyFKDeleteActions(ctx, txn, sess, parentTable, psch, pcells, 0)
}

// enforceFKParentUpdate applies the ON UPDATE referential action of every child
// FK when a parent row's referenced columns change — RESTRICT/NO ACTION block,
// CASCADE rewrites the child FK columns to the new values, SET NULL nulls them.
func (e *execImpl) enforceFKParentUpdate(ctx context.Context, txn *transactions.Txn, sess *session.Session, parentTable string, psch *tableSchema, oldCells, newCells []Datum) error {
	return e.applyFKUpdateActions(ctx, txn, sess, parentTable, psch, oldCells, newCells)
}

// refTupleFromParent returns the parent row's values for an FK's referenced
// columns. ok=false when any referenced column is NULL (no child can match a
// NULL-containing key under MATCH SIMPLE), so the cascade can skip it.
func refTupleFromParent(psch *tableSchema, pcells []Datum, fk fkConstraint) (vals []string, ok bool) {
	vals = make([]string, len(fk.pairs))
	for k, p := range fk.pairs {
		ref := p.Ref
		ci := -1
		if ref == "" {
			ci = psch.PKIndex // reference to the parent PK
		} else {
			ci = psch.colIndex(ref)
		}
		if ci < 0 || pcells[ci].Null {
			return nil, false
		}
		vals[k] = pcells[ci].Text
	}
	return vals, true
}

// childTupleMatches reports whether a child row's FK columns equal the referenced
// parent tuple (all pairs must match; a NULL FK column never matches).
func childTupleMatches(csch *tableSchema, ccells []Datum, fk fkConstraint, refVals []string) bool {
	for k, p := range fk.pairs {
		ci := csch.colIndex(p.Local)
		if ci < 0 || ccells[ci].Null || ccells[ci].Text != refVals[k] {
			return false
		}
	}
	return true
}

// canonicalFKAction normalizes an FK action string; "" means the SQL default of
// NO ACTION (which, like RESTRICT here, blocks).
func canonicalFKAction(a string) string {
	return strings.ToUpper(strings.TrimSpace(a))
}

// maxFKCascadeDepth bounds ON DELETE CASCADE recursion to guard against a cyclic
// FK graph causing unbounded recursion.
const maxFKCascadeDepth = 64

// applyFKDeleteActions enforces/propagates ON DELETE actions for one deleted
// parent row. depth guards cyclic cascades. A child row is affected iff its FULL
// FK tuple equals the parent row's referenced tuple (composite-aware).
func (e *execImpl) applyFKDeleteActions(ctx context.Context, txn *transactions.Txn, sess *session.Session, parentTable string, psch *tableSchema, pcells []Datum, depth int) error {
	if depth > maxFKCascadeDepth {
		return newExecError("54001", "ON DELETE CASCADE exceeded maximum depth (possible FK cycle)")
	}
	tables, err := e.listTables(ctx, txn, sess)
	if err != nil {
		return err
	}
	for _, t := range tables {
		for _, fk := range childForeignKeys(t) {
			if !strings.EqualFold(fk.table, parentTable) {
				continue
			}
			refVals, ok := refTupleFromParent(psch, pcells, fk)
			if !ok {
				continue // parent referenced tuple has a NULL: no child can match
			}
			action := canonicalFKAction(t.Cols[fk.anchor].FKOnDelete)
			sc, err := e.scanTable(ctx, txn, sess, t.Name, t.Name)
			if err != nil {
				return err
			}
			defs, err := e.loadIndexes(ctx, txn, sess, t.Name)
			if err != nil {
				return err
			}
			onBranch := sess.Branch() != "main"
			for _, r := range sc.rows {
				if !childTupleMatches(t, r.cells, fk, refVals) {
					continue
				}
				switch action {
				case "CASCADE":
					// Recurse first so grandchildren are handled before the child
					// row's key disappears.
					if err := e.applyFKDeleteActions(ctx, txn, sess, t.Name, t, r.cells, depth+1); err != nil {
						return err
					}
					bk := e.branchRowKey(sess, t.Name, t, r.cells, r.key)
					if onBranch {
						e.txn.Buffer(txn, transactions.Mutation{Key: bk, Value: storage.Tombstone()})
					} else {
						e.txn.Buffer(txn, transactions.Mutation{Key: bk, Delete: true})
					}
					if err := e.indexEntries(txn, sess, t, defs, r.cells, r.key, false); err != nil {
						return err
					}
				case "SET NULL":
					newCells := append([]Datum(nil), r.cells...)
					for _, p := range fk.pairs {
						newCells[t.colIndex(p.Local)] = Datum{Null: true}
					}
					bk := e.branchRowKey(sess, t.Name, t, newCells, r.key)
					if err := e.indexEntries(txn, sess, t, defs, r.cells, r.key, false); err != nil {
						return err
					}
					e.txn.Buffer(txn, transactions.Mutation{Key: bk, Value: encodeRow(newCells)})
					if err := e.indexEntries(txn, sess, t, defs, newCells, r.key, true); err != nil {
						return err
					}
				default: // RESTRICT / NO ACTION / SET DEFAULT (treated as restrict)
					// A DEFERRABLE FK deferred in this txn postpones the RESTRICT
					// check to COMMIT: record the orphaned parent key and continue
					// (the child row stays, but its validity is rechecked later).
					if e.effectiveDeferral(ctx, fk) {
						e.deferred.stateFor(txn).record(pendingFKCheck{
							kind:           pendingFKParentRestrict,
							constraint:     fk.name,
							parentTab:      parentTable,
							childTabFilter: t.Name,
							fkAnchor:       fk.anchor,
							vals:           append([]string(nil), refVals...),
						})
						continue
					}
					return newExecError("23503",
						"update or delete on table %q violates foreign key constraint on table %q",
						parentTable, t.Name)
				}
			}
		}
	}
	return nil
}

// applyFKUpdateActions enforces/propagates ON UPDATE actions for one changed
// parent row, matching child rows on the full referenced tuple and rewriting all
// FK columns of the key (composite-aware).
func (e *execImpl) applyFKUpdateActions(ctx context.Context, txn *transactions.Txn, sess *session.Session, parentTable string, psch *tableSchema, oldCells, newCells []Datum) error {
	tables, err := e.listTables(ctx, txn, sess)
	if err != nil {
		return err
	}
	for _, t := range tables {
		for _, fk := range childForeignKeys(t) {
			if !strings.EqualFold(fk.table, parentTable) {
				continue
			}
			oldVals, ok := refTupleFromParent(psch, oldCells, fk)
			if !ok {
				continue
			}
			newVals, _ := refTupleFromParent(psch, newCells, fk)
			action := canonicalFKAction(t.Cols[fk.anchor].FKOnUpdate)
			sc, err := e.scanTable(ctx, txn, sess, t.Name, t.Name)
			if err != nil {
				return err
			}
			defs, err := e.loadIndexes(ctx, txn, sess, t.Name)
			if err != nil {
				return err
			}
			for _, r := range sc.rows {
				if !childTupleMatches(t, r.cells, fk, oldVals) {
					continue
				}
				switch action {
				case "CASCADE", "SET NULL":
					rowCells := append([]Datum(nil), r.cells...)
					for k, p := range fk.pairs {
						ci := t.colIndex(p.Local)
						if action == "SET NULL" {
							rowCells[ci] = Datum{Null: true}
						} else {
							rowCells[ci] = Datum{Text: newVals[k]}
						}
					}
					bk := e.branchRowKey(sess, t.Name, t, rowCells, r.key)
					if err := e.indexEntries(txn, sess, t, defs, r.cells, r.key, false); err != nil {
						return err
					}
					e.txn.Buffer(txn, transactions.Mutation{Key: bk, Value: encodeRow(rowCells)})
					if err := e.indexEntries(txn, sess, t, defs, rowCells, r.key, true); err != nil {
						return err
					}
				default: // RESTRICT / NO ACTION
					if e.effectiveDeferral(ctx, fk) {
						e.deferred.stateFor(txn).record(pendingFKCheck{
							kind:           pendingFKParentRestrict,
							constraint:     fk.name,
							parentTab:      parentTable,
							childTabFilter: t.Name,
							fkAnchor:       fk.anchor,
							vals:           append([]string(nil), oldVals...),
						})
						continue
					}
					return newExecError("23503",
						"update or delete on table %q violates foreign key constraint on table %q",
						parentTable, t.Name)
				}
			}
		}
	}
	return nil
}
