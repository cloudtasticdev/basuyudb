package executor

import (
	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
)

// evalScalarSub evaluates a scalar subquery `(SELECT ...)`. It must return at
// most one row of one column; zero rows yield NULL, more than one row is an
// error (uncorrelated only — the subquery cannot see the outer row).
func (ev *evaluator) evalScalarSub(s *ast.SubLink) (value, error) {
	sel, ok := s.SubSelect.(*ast.SelectStmt)
	if !ok || ev.runSub == nil {
		return value{}, newExecError("0A000", "subquery is not supported in this position")
	}
	res, err := ev.runSub(sel)
	if err != nil {
		return value{}, err
	}
	if len(res.Rows) == 0 {
		return value{null: true, oid: OIDText}, nil
	}
	if len(res.Rows) > 1 {
		return value{}, newExecError("21000", "scalar subquery returned more than one row")
	}
	if len(res.Rows[0]) == 0 {
		return value{null: true, oid: OIDText}, nil
	}
	cell := res.Rows[0][0]
	oid := OIDText
	if len(res.Columns) > 0 {
		oid = res.Columns[0].TypeOID
	}
	return value{null: cell.Null, text: cell.Text, oid: oid}, nil
}

// evalIn evaluates `expr IN (...)` where the right side is a value list or a
// subquery. SQL three-valued logic: NULL on the left, or no match with a NULL
// present, yields NULL; otherwise a boolean.
func (ev *evaluator) evalIn(e *ast.A_Expr) (value, error) {
	lv, err := ev.eval(e.Lexpr)
	if err != nil {
		return value{}, err
	}
	if lv.null {
		return value{null: true, oid: OIDBool}, nil
	}

	var candidates []value
	switch r := e.Rexpr.(type) {
	case *ast.List:
		for _, it := range r.Items {
			v, err := ev.eval(it)
			if err != nil {
				return value{}, err
			}
			candidates = append(candidates, v)
		}
	case *ast.SubLink:
		sel, ok := r.SubSelect.(*ast.SelectStmt)
		if !ok || ev.runSub == nil {
			return value{}, newExecError("0A000", "IN subquery is not supported in this position")
		}
		res, err := ev.runSub(sel)
		if err != nil {
			return value{}, err
		}
		for _, row := range res.Rows {
			if len(row) == 0 {
				continue
			}
			candidates = append(candidates, value{null: row[0].Null, text: row[0].Text, oid: firstColOID(res)})
		}
	default:
		return value{}, newExecError("0A000", "unsupported right-hand side for IN")
	}

	sawNull := false
	for _, c := range candidates {
		if c.null {
			sawNull = true
			continue
		}
		eq, err := compare("=", lv, c)
		if err != nil {
			return value{}, err
		}
		if asBool(eq) {
			return value{text: "t", oid: OIDBool}, nil
		}
	}
	if sawNull {
		// No match but a NULL was present: SQL yields UNKNOWN (NULL).
		return value{null: true, oid: OIDBool}, nil
	}
	return value{text: "f", oid: OIDBool}, nil
}

func firstColOID(res *Result) uint32 {
	if len(res.Columns) > 0 {
		return res.Columns[0].TypeOID
	}
	return OIDText
}
