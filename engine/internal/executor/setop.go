package executor

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
)

// execSetOp evaluates a UNION / INTERSECT / EXCEPT node: it runs both operands,
// combines their rows with the right multiset/set semantics, then applies the
// node's own ORDER BY / LIMIT / OFFSET to the combined result.
func (e *execImpl) execSetOp(ctx context.Context, s *ast.SelectStmt, sess *session.Session, params []Datum) (*Result, error) {
	left, err := e.execSelect(ctx, s.Larg, sess, params)
	if err != nil {
		return nil, err
	}
	right, err := e.execSelect(ctx, s.Rarg, sess, params)
	if err != nil {
		return nil, err
	}
	if len(left.Columns) != len(right.Columns) {
		return nil, newExecError("42601", "each %s query must have the same number of columns", setOpName(s.SetOp))
	}

	var rows [][]Datum
	switch s.SetOp {
	case ast.SetOpUnion:
		rows = append(append(rows, left.Rows...), right.Rows...)
		if !s.All {
			rows = dedupRows(rows)
		}
	case ast.SetOpIntersect:
		rows = intersectRows(left.Rows, right.Rows, s.All)
	case ast.SetOpExcept:
		rows = exceptRows(left.Rows, right.Rows, s.All)
	default:
		return nil, newExecError("XX000", "unknown set operation")
	}

	res := &Result{Columns: left.Columns, Rows: rows, Command: "SELECT"}

	// ORDER BY / LIMIT / OFFSET apply to the combined result.
	if len(s.SortClause) > 0 {
		if err := sortResultRows(res, s.SortClause, params); err != nil {
			return nil, err
		}
	}
	applyOffsetLimit(res, s, params)
	return res, nil
}

// datumRowKey builds a content key for a row with SQL set semantics (NULLs are
// equal to one another).
func datumRowKey(row []Datum) string {
	var b strings.Builder
	for _, d := range row {
		if d.Null {
			b.WriteString("\x00N|")
		} else {
			b.WriteByte(1)
			b.WriteString(d.Text)
			b.WriteByte('|')
		}
	}
	return b.String()
}

// intersectRows returns rows present in both inputs. With all=false the result
// is a set (deduped); with all=true it is a multiset (min of the two counts).
func intersectRows(left, right [][]Datum, all bool) [][]Datum {
	rc := make(map[string]int, len(right))
	for _, r := range right {
		rc[datumRowKey(r)]++
	}
	var out [][]Datum
	seen := map[string]bool{}
	for _, r := range left {
		k := datumRowKey(r)
		if rc[k] <= 0 {
			continue
		}
		if all {
			rc[k]--
			out = append(out, r)
		} else if !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	return out
}

// exceptRows returns left rows not present in right. With all=false the result
// is a set; with all=true it is a multiset difference.
func exceptRows(left, right [][]Datum, all bool) [][]Datum {
	rc := make(map[string]int, len(right))
	for _, r := range right {
		rc[datumRowKey(r)]++
	}
	var out [][]Datum
	seen := map[string]bool{}
	for _, r := range left {
		k := datumRowKey(r)
		if all {
			if rc[k] > 0 {
				rc[k]--
				continue
			}
			out = append(out, r)
		} else {
			if rc[k] > 0 || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, r)
		}
	}
	return out
}

// sortResultRows sorts a materialized Result in place by ORDER BY items that
// reference output columns — by 1-based ordinal (ORDER BY 2) or by output column
// name (ORDER BY city). Comparison is numeric when both values parse as numbers,
// else lexicographic; NULLs sort last.
func sortResultRows(res *Result, sortClause []*ast.SortBy, params []Datum) error {
	idxs := make([]int, len(sortClause))
	for i, sb := range sortClause {
		col, err := resolveSortColumn(sb.Node, res.Columns, params)
		if err != nil {
			return err
		}
		idxs[i] = col
	}
	sort.SliceStable(res.Rows, func(a, b int) bool {
		for k, ci := range idxs {
			c := compareDatum(res.Rows[a][ci], res.Rows[b][ci])
			if c == 0 {
				continue
			}
			if sortClause[k].SortDir == 2 { // DESC
				return c > 0
			}
			return c < 0
		}
		return false
	})
	return nil
}

// resolveSortColumn maps an ORDER BY item to an output column index.
func resolveSortColumn(n ast.Node, cols []Column, params []Datum) (int, error) {
	switch e := n.(type) {
	case *ast.A_Const:
		if e.Type == ast.ConstInt {
			pos, err := strconv.Atoi(e.Val)
			if err == nil && pos >= 1 && pos <= len(cols) {
				return pos - 1, nil
			}
		}
	case *ast.ColumnRef:
		if len(e.Fields) > 0 {
			name := e.Fields[len(e.Fields)-1]
			for i, c := range cols {
				if strings.EqualFold(c.Name, name) {
					return i, nil
				}
			}
		}
	}
	return 0, newExecError("42P10", "ORDER BY on a set operation must reference an output column name or position")
}

// compareDatum orders two datums: NULLs last, numeric when both numeric, else
// lexicographic. Returns -1, 0, or 1.
func compareDatum(a, b Datum) int {
	if a.Null || b.Null {
		switch {
		case a.Null && b.Null:
			return 0
		case a.Null:
			return 1 // NULLs sort last
		default:
			return -1
		}
	}
	af, aerr := strconv.ParseFloat(a.Text, 64)
	bf, berr := strconv.ParseFloat(b.Text, 64)
	if aerr == nil && berr == nil {
		switch {
		case af < bf:
			return -1
		case af > bf:
			return 1
		default:
			return 0
		}
	}
	return strings.Compare(a.Text, b.Text)
}

// applyOffsetLimit applies OFFSET then LIMIT to a materialized Result in place.
func applyOffsetLimit(res *Result, s *ast.SelectStmt, params []Datum) {
	if s.LimitOffset != nil {
		if off, err := evalIntLimit(s.LimitOffset, params); err == nil && off > 0 {
			if off >= len(res.Rows) {
				res.Rows = nil
			} else {
				res.Rows = res.Rows[off:]
			}
		}
	}
	if s.LimitCount != nil {
		if lim, err := evalIntLimit(s.LimitCount, params); err == nil && lim >= 0 && lim < len(res.Rows) {
			res.Rows = res.Rows[:lim]
		}
	}
}

func setOpName(op ast.SetOpType) string {
	switch op {
	case ast.SetOpUnion:
		return "UNION"
	case ast.SetOpIntersect:
		return "INTERSECT"
	case ast.SetOpExcept:
		return "EXCEPT"
	}
	return "set"
}
