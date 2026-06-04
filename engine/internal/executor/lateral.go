package executor

import (
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
)

// outerScope maps a lower-cased column key (either "alias.col", "table.col", or
// bare "col") to the literal value from a preceding (left) FROM row. It is used
// to rewrite a LATERAL subquery's correlated column references into constants so
// the subquery can be executed as an ordinary (uncorrelated) query for that row.
type outerScope struct {
	qualified map[string]value // "alias.col" / "schema.col"
	bare      map[string]value // "col" — present only when unambiguous across bindings
	ambiguous map[string]bool  // bare names appearing in >1 binding (do not substitute)
}

// buildOuterScope captures every left-binding column value as a substitutable
// outer reference.
func buildOuterScope(left boundRow) *outerScope {
	sc := &outerScope{
		qualified: map[string]value{},
		bare:      map[string]value{},
		ambiguous: map[string]bool{},
	}
	for _, b := range left {
		for i, col := range b.schema.Cols {
			v := cellValue(b, i)
			lc := strings.ToLower(col.Name)
			if b.alias != "" {
				sc.qualified[strings.ToLower(b.alias)+"."+lc] = v
			}
			if b.schema.Name != "" {
				sc.qualified[strings.ToLower(b.schema.Name)+"."+lc] = v
			}
			if _, seen := sc.bare[lc]; seen {
				sc.ambiguous[lc] = true
			} else {
				sc.bare[lc] = v
			}
		}
	}
	return sc
}

// lookup returns the outer value for a column reference's fields, or ok=false if
// the reference is not an outer (left) column (so it belongs to the subquery's
// own FROM and must be left intact).
func (sc *outerScope) lookup(fields []string) (value, bool) {
	if len(fields) == 0 {
		return value{}, false
	}
	if len(fields) >= 2 {
		key := strings.ToLower(fields[len(fields)-2]) + "." + strings.ToLower(fields[len(fields)-1])
		v, ok := sc.qualified[key]
		return v, ok
	}
	col := strings.ToLower(fields[0])
	if sc.ambiguous[col] {
		return value{}, false
	}
	v, ok := sc.bare[col]
	return v, ok
}

// valueToConst renders an outer value as an A_Const literal for substitution.
func valueToConst(v value) *ast.A_Const {
	if v.null {
		return &ast.A_Const{Type: ast.ConstNull}
	}
	return &ast.A_Const{Type: ast.ConstString, Val: v.text}
}

// substituteOuter returns a deep copy of n with every correlated (outer) column
// reference replaced by the corresponding literal from scope. Non-outer column
// references (the subquery's own columns) are preserved. Only the node kinds that
// can appear inside a LATERAL subquery are cloned; an unrecognized kind is
// returned as-is (shared) — acceptable because such nodes carry no correlated
// refs we rewrite.
func substituteOuter(n ast.Node, scope *outerScope) ast.Node {
	switch e := n.(type) {
	case nil:
		return nil
	case *ast.ColumnRef:
		if v, ok := scope.lookup(e.Fields); ok {
			return valueToConst(v)
		}
		return e
	case *ast.A_Const, *ast.ParamRef, *ast.A_Star:
		return e
	case *ast.A_Expr:
		return &ast.A_Expr{
			Kind:  e.Kind,
			Name:  e.Name,
			Lexpr: substituteOuter(e.Lexpr, scope),
			Rexpr: substituteOuter(e.Rexpr, scope),
		}
	case *ast.BoolExpr:
		args := make([]ast.Node, len(e.Args))
		for i, a := range e.Args {
			args[i] = substituteOuter(a, scope)
		}
		return &ast.BoolExpr{Op: e.Op, Args: args}
	case *ast.NullTest:
		return &ast.NullTest{Arg: substituteOuter(e.Arg, scope), TestNull: e.TestNull}
	case *ast.TypeCast:
		return &ast.TypeCast{Arg: substituteOuter(e.Arg, scope), TypeName: e.TypeName}
	case *ast.List:
		items := make([]ast.Node, len(e.Items))
		for i, it := range e.Items {
			items[i] = substituteOuter(it, scope)
		}
		return &ast.List{Items: items}
	case *ast.FuncCall:
		args := make([]ast.Node, len(e.Args))
		for i, a := range e.Args {
			args[i] = substituteOuter(a, scope)
		}
		cp := *e
		cp.Args = args
		return &cp
	case *ast.CaseExpr:
		cp := *e
		cp.Arg = substituteOuter(e.Arg, scope)
		cp.Else = substituteOuter(e.Else, scope)
		cp.Whens = make([]*ast.CaseWhen, len(e.Whens))
		for i, w := range e.Whens {
			cp.Whens[i] = &ast.CaseWhen{
				Cond:   substituteOuter(w.Cond, scope),
				Result: substituteOuter(w.Result, scope),
			}
		}
		return &cp
	case *ast.SubLink:
		cp := *e
		if sel, ok := e.SubSelect.(*ast.SelectStmt); ok {
			cp.SubSelect = substituteOuterSelect(sel, scope)
		}
		return &cp
	case *ast.SelectStmt:
		return substituteOuterSelect(e, scope)
	default:
		return n
	}
}

// substituteOuterSelect deep-copies a SELECT, substituting outer references in
// its target list, WHERE, GROUP BY, HAVING, ORDER BY, and LIMIT. The FROM clause
// is preserved (its tables define the subquery's own scope); column references
// that match an inner table are left intact by substituteOuter via the scope's
// non-membership.
func substituteOuterSelect(s *ast.SelectStmt, scope *outerScope) *ast.SelectStmt {
	cp := *s
	if len(s.TargetList) > 0 {
		cp.TargetList = make([]*ast.ResTarget, len(s.TargetList))
		for i, t := range s.TargetList {
			nt := *t
			nt.Val = substituteOuter(t.Val, scope)
			cp.TargetList[i] = &nt
		}
	}
	cp.WhereClause = substituteOuter(s.WhereClause, scope)
	cp.HavingClause = substituteOuter(s.HavingClause, scope)
	if len(s.GroupClause) > 0 {
		cp.GroupClause = make([]ast.Node, len(s.GroupClause))
		for i, g := range s.GroupClause {
			cp.GroupClause[i] = substituteOuter(g, scope)
		}
	}
	if len(s.SortClause) > 0 {
		cp.SortClause = make([]*ast.SortBy, len(s.SortClause))
		for i, sb := range s.SortClause {
			nsb := *sb
			nsb.Node = substituteOuter(sb.Node, scope)
			cp.SortClause[i] = &nsb
		}
	}
	cp.LimitCount = substituteOuter(s.LimitCount, scope)
	cp.LimitOffset = substituteOuter(s.LimitOffset, scope)
	return &cp
}
