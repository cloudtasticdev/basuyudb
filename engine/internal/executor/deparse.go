package executor

import (
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
)

// deparseSelect renders a SELECT statement back to SQL text so a view definition
// can be persisted and re-parsed. It covers the supported query surface and
// returns an error for anything it cannot faithfully reproduce — so an
// unsupported view fails at CREATE time rather than misbehaving later.
func deparseSelect(s *ast.SelectStmt) (string, error) {
	var b strings.Builder

	if s.WithClause != nil {
		ctes := make([]string, 0, len(s.WithClause.CTEs))
		for _, c := range s.WithClause.CTEs {
			q, ok := c.Query.(*ast.SelectStmt)
			if !ok {
				return "", newExecError("0A000", "unsupported CTE in view")
			}
			sub, err := deparseSelect(q)
			if err != nil {
				return "", err
			}
			ctes = append(ctes, c.Name+" AS ("+sub+")")
		}
		b.WriteString("WITH " + strings.Join(ctes, ", ") + " ")
	}

	// Set-operation node: render both operands joined by the operator.
	if s.SetOp != ast.SetOpNone {
		l, err := deparseSelect(s.Larg)
		if err != nil {
			return "", err
		}
		r, err := deparseSelect(s.Rarg)
		if err != nil {
			return "", err
		}
		op := setOpName(s.SetOp)
		if s.All {
			op += " ALL"
		}
		b.WriteString(l + " " + op + " " + r)
	} else {
		b.WriteString("SELECT ")
		if s.Distinct {
			b.WriteString("DISTINCT ")
		}
		tl, err := deparseTargetList(s.TargetList)
		if err != nil {
			return "", err
		}
		b.WriteString(tl)
		if len(s.FromClause) > 0 {
			fi, err := deparseFromItem(s.FromClause[0])
			if err != nil {
				return "", err
			}
			b.WriteString(" FROM " + fi)
		}
		if s.WhereClause != nil {
			w, err := deparseExpr(s.WhereClause)
			if err != nil {
				return "", err
			}
			b.WriteString(" WHERE " + w)
		}
		if len(s.GroupClause) > 0 {
			gs := make([]string, 0, len(s.GroupClause))
			for _, g := range s.GroupClause {
				e, err := deparseExpr(g)
				if err != nil {
					return "", err
				}
				gs = append(gs, e)
			}
			b.WriteString(" GROUP BY " + strings.Join(gs, ", "))
		}
		if s.HavingClause != nil {
			h, err := deparseExpr(s.HavingClause)
			if err != nil {
				return "", err
			}
			b.WriteString(" HAVING " + h)
		}
	}

	if len(s.SortClause) > 0 {
		ob, err := deparseSortList(s.SortClause)
		if err != nil {
			return "", err
		}
		b.WriteString(" ORDER BY " + ob)
	}
	if s.LimitCount != nil {
		l, err := deparseExpr(s.LimitCount)
		if err != nil {
			return "", err
		}
		b.WriteString(" LIMIT " + l)
	}
	if s.LimitOffset != nil {
		o, err := deparseExpr(s.LimitOffset)
		if err != nil {
			return "", err
		}
		b.WriteString(" OFFSET " + o)
	}
	return b.String(), nil
}

func deparseTargetList(targets []*ast.ResTarget) (string, error) {
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		e, err := deparseExpr(t.Val)
		if err != nil {
			return "", err
		}
		if t.Name != "" {
			e += " AS " + t.Name
		}
		parts = append(parts, e)
	}
	return strings.Join(parts, ", "), nil
}

func deparseSortList(sorts []*ast.SortBy) (string, error) {
	parts := make([]string, 0, len(sorts))
	for _, sb := range sorts {
		e, err := deparseExpr(sb.Node)
		if err != nil {
			return "", err
		}
		switch sb.SortDir {
		case 1:
			e += " ASC"
		case 2:
			e += " DESC"
		}
		parts = append(parts, e)
	}
	return strings.Join(parts, ", "), nil
}

func deparseFromItem(n ast.Node) (string, error) {
	switch f := n.(type) {
	case *ast.RangeVar:
		name := f.RelName
		if f.SchemaName != "" {
			name = f.SchemaName + "." + name
		}
		if f.Alias != nil && f.Alias.AliasName != "" {
			name += " AS " + f.Alias.AliasName
		}
		return name, nil
	case *ast.JoinExpr:
		l, err := deparseFromItem(f.Larg)
		if err != nil {
			return "", err
		}
		r, err := deparseFromItem(f.Rarg)
		if err != nil {
			return "", err
		}
		kw := "JOIN"
		switch f.JoinType {
		case ast.JOIN_LEFT:
			kw = "LEFT JOIN"
		case ast.JOIN_RIGHT:
			kw = "RIGHT JOIN"
		case ast.JOIN_FULL:
			kw = "FULL JOIN"
		case ast.JOIN_CROSS:
			kw = "CROSS JOIN"
		}
		out := l + " " + kw + " " + r
		if len(f.UsingCols) > 0 {
			out += " USING (" + strings.Join(f.UsingCols, ", ") + ")"
		} else if f.Quals != nil {
			q, err := deparseExpr(f.Quals)
			if err != nil {
				return "", err
			}
			out += " ON " + q
		}
		return out, nil
	case *ast.SubLink:
		sub, ok := f.SubSelect.(*ast.SelectStmt)
		if !ok {
			return "", newExecError("0A000", "unsupported subquery FROM item in view")
		}
		s, err := deparseSelect(sub)
		if err != nil {
			return "", err
		}
		return "(" + s + ")", nil
	default:
		return "", newExecError("0A000", "unsupported FROM item %T in view", n)
	}
}
