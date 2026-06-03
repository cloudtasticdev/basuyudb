package executor

import (
	"bytes"
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
)

// isAggregateName reports whether a function name is a supported SQL aggregate.
func isAggregateName(n string) bool {
	switch n {
	case "count", "sum", "avg", "min", "max":
		return true
	}
	return false
}

// containsAggregate reports whether an expression tree contains an aggregate
// function call.
func containsAggregate(n ast.Node) bool {
	if n == nil {
		return false
	}
	found := false
	_ = ast.Walk(n, func(node ast.Node) error {
		if fc, ok := node.(*ast.FuncCall); ok {
			// A windowed aggregate (f(...) OVER (...)) is not a grouping aggregate.
			if fc.Over == nil && isAggregateName(strings.ToLower(strings.Join(fc.FuncName, "."))) {
				found = true
			}
		}
		return nil
	})
	return found
}

// needsAggregation reports whether a SELECT requires the grouping path: it has a
// GROUP BY, or any target-list expression contains an aggregate.
func needsAggregation(s *ast.SelectStmt) bool {
	if len(s.GroupClause) > 0 {
		return true
	}
	for _, t := range s.TargetList {
		if containsAggregate(t.Val) {
			return true
		}
	}
	return false
}

// evalAggregate reduces one aggregate call over a group's rows.
func evalAggregate(name string, f *ast.FuncCall, group []boundRow, params []Datum) (value, error) {
	if name == "count" && f.AggStar {
		return value{text: strconv.Itoa(len(group)), oid: OIDInt8}, nil
	}
	if len(f.Args) != 1 {
		return value{}, newExecError("42883", "aggregate %q expects exactly one argument", name)
	}
	arg := f.Args[0]

	// Collect the non-NULL argument values across the group.
	var vals []value
	for _, row := range group {
		rev := &evaluator{params: params, resolveCol: combinedResolver(row)}
		v, err := rev.eval(arg)
		if err != nil {
			return value{}, err
		}
		if !v.null {
			vals = append(vals, v)
		}
	}

	switch name {
	case "count":
		return value{text: strconv.Itoa(len(vals)), oid: OIDInt8}, nil

	case "sum":
		if len(vals) == 0 {
			return value{null: true, oid: OIDInt8}, nil
		}
		// Integer sum when every value is an integer; otherwise float.
		var isum int64
		intOK := true
		for _, v := range vals {
			n, err := strconv.ParseInt(v.text, 10, 64)
			if err != nil {
				intOK = false
				break
			}
			isum += n
		}
		if intOK {
			return value{text: strconv.FormatInt(isum, 10), oid: OIDInt8}, nil
		}
		var fsum float64
		for _, v := range vals {
			fv, err := strconv.ParseFloat(v.text, 64)
			if err != nil {
				return value{}, newExecError("22P02", "sum over non-numeric value %q", v.text)
			}
			fsum += fv
		}
		return value{text: strconv.FormatFloat(fsum, 'g', -1, 64), oid: OIDFloat8}, nil

	case "avg":
		if len(vals) == 0 {
			return value{null: true, oid: OIDFloat8}, nil
		}
		var fsum float64
		for _, v := range vals {
			fv, err := strconv.ParseFloat(v.text, 64)
			if err != nil {
				return value{}, newExecError("22P02", "avg over non-numeric value %q", v.text)
			}
			fsum += fv
		}
		return value{text: strconv.FormatFloat(fsum/float64(len(vals)), 'g', -1, 64), oid: OIDFloat8}, nil

	case "min", "max":
		if len(vals) == 0 {
			return value{null: true, oid: OIDText}, nil
		}
		best := vals[0]
		bestKey, err := orderEncode(best.oid, best.text)
		if err != nil {
			return value{}, err
		}
		for _, v := range vals[1:] {
			k, err := orderEncode(v.oid, v.text)
			if err != nil {
				return value{}, err
			}
			c := bytes.Compare(k, bestKey)
			if (name == "min" && c < 0) || (name == "max" && c > 0) {
				best, bestKey = v, k
			}
		}
		return best, nil
	}
	return value{}, newExecError("42883", "unknown aggregate %q", name)
}

// execAggregate groups the (already WHERE-filtered) rows, applies HAVING, and
// projects the target list per group. Supports GROUP BY, the five core
// aggregates, HAVING, ORDER BY over grouped output, and LIMIT.
func (e *execImpl) execAggregate(ctx context.Context, sess *session.Session, s *ast.SelectStmt, rows []boundRow, params []Datum) (*Result, error) {
	// Partition rows into groups, preserving first-seen group order.
	type bucket struct {
		rep  boundRow // representative row for resolving GROUP BY columns
		rows []boundRow
	}
	order := []string{}
	groups := map[string]*bucket{}
	add := func(key string, row boundRow) {
		b, ok := groups[key]
		if !ok {
			b = &bucket{rep: row}
			groups[key] = b
			order = append(order, key)
		}
		b.rows = append(b.rows, row)
	}

	if len(s.GroupClause) == 0 {
		// No GROUP BY: a single group over all rows (one output row even if empty).
		for _, r := range rows {
			add("", r)
		}
		if len(rows) == 0 {
			groups[""] = &bucket{rep: nil, rows: nil}
			order = []string{""}
		}
	} else {
		for _, r := range rows {
			key, err := groupKey(s.GroupClause, r, params)
			if err != nil {
				return nil, err
			}
			add(key, r)
		}
	}

	// Project + HAVING per group, tracking sort keys for ORDER BY.
	type outRow struct {
		cells    []Datum
		sortKeys [][]byte
		sortNull []bool
	}
	var cols []Column
	var out []outRow
	for _, key := range order {
		b := groups[key]
		grp := b.rows
		if grp == nil {
			grp = []boundRow{} // non-nil so the evaluator knows it is aggregating
		}
		gev := &evaluator{params: params, group: grp}
		if b.rep != nil {
			gev.resolveCol = combinedResolver(b.rep)
		}

		if s.HavingClause != nil {
			hv, err := gev.eval(s.HavingClause)
			if err != nil {
				return nil, err
			}
			if !asBool(hv) {
				continue
			}
		}

		cells := make([]Datum, len(s.TargetList))
		rowCols := make([]Column, len(s.TargetList))
		for i, t := range s.TargetList {
			v, err := gev.eval(t.Val)
			if err != nil {
				return nil, err
			}
			cells[i] = Datum{Null: v.null, Text: v.text}
			name := t.Name
			if name == "" {
				name = defaultColName(t.Val, i)
			}
			rowCols[i] = Column{Name: name, TypeOID: v.oid}
		}
		if cols == nil {
			cols = rowCols
		}

		o := outRow{cells: cells}
		if len(s.SortClause) > 0 {
			o.sortKeys = make([][]byte, len(s.SortClause))
			o.sortNull = make([]bool, len(s.SortClause))
			for j, sb := range s.SortClause {
				v, err := gev.eval(sb.Node)
				if err != nil {
					return nil, err
				}
				if v.null {
					o.sortNull[j] = true
					continue
				}
				enc, err := orderEncode(v.oid, v.text)
				if err != nil {
					return nil, err
				}
				o.sortKeys[j] = enc
			}
		}
		out = append(out, o)
	}

	// ORDER BY over grouped output (keys precomputed per group above).
	if len(s.SortClause) > 0 {
		sort.SliceStable(out, func(a, b int) bool {
			for j, sb := range s.SortClause {
				c := compareSortKey(out[a].sortNull[j], out[a].sortKeys[j], out[b].sortNull[j], out[b].sortKeys[j])
				if c == 0 {
					continue
				}
				if sb.SortDir == 2 { // DESC
					return c > 0
				}
				return c < 0
			}
			return false
		})
	}

	// DISTINCT over aggregated output: dedup projected rows before LIMIT.
	if s.Distinct {
		seen := make(map[string]bool, len(out))
		kept := out[:0:0]
		for _, o := range out {
			var b strings.Builder
			for _, d := range o.cells {
				if d.Null {
					b.WriteString("\x00N|")
				} else {
					b.WriteByte(1)
					b.WriteString(d.Text)
					b.WriteByte('|')
				}
			}
			k := b.String()
			if seen[k] {
				continue
			}
			seen[k] = true
			kept = append(kept, o)
		}
		out = kept
	}

	// LIMIT.
	if s.LimitCount != nil {
		lim, err := evalIntLimit(s.LimitCount, params)
		if err != nil {
			return nil, err
		}
		if lim >= 0 && lim < len(out) {
			out = out[:lim]
		}
	}

	rowsOut := make([][]Datum, len(out))
	for i, o := range out {
		rowsOut[i] = o.cells
	}
	if cols == nil {
		// Aggregation with no surviving groups: derive column names from targets.
		cols = make([]Column, len(s.TargetList))
		for i, t := range s.TargetList {
			name := t.Name
			if name == "" {
				name = defaultColName(t.Val, i)
			}
			cols[i] = Column{Name: name, TypeOID: OIDText}
		}
	}
	return &Result{Columns: cols, Rows: rowsOut, Command: "SELECT"}, nil
}

// groupKey builds a stable grouping key from the GROUP BY expressions for a row.
func groupKey(clause []ast.Node, row boundRow, params []Datum) (string, error) {
	ev := &evaluator{params: params, resolveCol: combinedResolver(row)}
	var buf bytes.Buffer
	for _, g := range clause {
		v, err := ev.eval(g)
		if err != nil {
			return "", err
		}
		if v.null {
			buf.WriteByte(0)
			buf.WriteString("\x00NULL\x00")
			continue
		}
		enc, err := orderEncode(v.oid, v.text)
		if err != nil {
			return "", err
		}
		buf.WriteByte(1)
		buf.Write(enc)
	}
	return buf.String(), nil
}
