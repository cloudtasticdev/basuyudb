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

// enforceFKChild verifies that every non-NULL foreign-key column in a candidate
// row references an existing parent row (referential integrity, child side).
func (e *execImpl) enforceFKChild(ctx context.Context, txn *transactions.Txn, sess *session.Session, sch *tableSchema, cells []Datum) error {
	for i, c := range sch.Cols {
		if c.FKTable == "" || cells[i].Null {
			continue
		}
		ok, err := e.parentRowExists(ctx, txn, sess, c.FKTable, c.FKColumn, cells[i].Text)
		if err != nil {
			return err
		}
		if !ok {
			return newExecError("23503",
				"insert or update on table violates foreign key constraint: key (%s)=(%s) is not present in table %q",
				c.Name, cells[i].Text, c.FKTable)
		}
	}
	return nil
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

// enforceFKParent verifies no child row references a parent key that is about to
// be deleted or have its key changed (ON DELETE/UPDATE RESTRICT, the default).
func (e *execImpl) enforceFKParent(ctx context.Context, txn *transactions.Txn, sess *session.Session, parentTable string, pkValue string) error {
	tables, err := e.listTables(ctx, txn, sess)
	if err != nil {
		return err
	}
	for _, t := range tables {
		for i, c := range t.Cols {
			if !strings.EqualFold(c.FKTable, parentTable) {
				continue
			}
			sc, err := e.scanTable(ctx, txn, sess, t.Name, t.Name)
			if err != nil {
				return err
			}
			for _, r := range sc.rows {
				if !r.cells[i].Null && r.cells[i].Text == pkValue {
					return newExecError("23503",
						"update or delete on table %q violates foreign key constraint on table %q",
						parentTable, t.Name)
				}
			}
		}
	}
	return nil
}
