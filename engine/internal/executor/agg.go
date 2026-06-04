package executor

import (
	"bytes"
	"context"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
)

// isAggregateName reports whether a function name is a supported SQL aggregate.
func isAggregateName(n string) bool {
	switch n {
	case "count", "sum", "avg", "min", "max",
		"array_agg", "string_agg", "json_agg", "jsonb_agg",
		"json_object_agg", "jsonb_object_agg",
		"bool_and", "bool_or", "every",
		"percentile_cont", "percentile_disc", "mode",
		"first", "last", "first_value", "last_value":
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
	// Apply FILTER (WHERE expr) — only rows matching the condition contribute.
	if f.Filter != nil {
		var filteredGroup []boundRow
		for _, row := range group {
			ev := &evaluator{params: params, resolveCol: combinedResolver(row)}
			cond, err := ev.eval(f.Filter)
			if err != nil || !asBool(cond) {
				continue
			}
			filteredGroup = append(filteredGroup, row)
		}
		group = filteredGroup
	}

	if name == "count" && f.AggStar {
		return value{text: strconv.Itoa(len(group)), oid: OIDInt8}, nil
	}

	// COUNT(DISTINCT col) / aggregate with DISTINCT: deduplicate on arg[0] value.
	if f.AggDistinct && len(f.Args) > 0 {
		seen := map[string]bool{}
		var dedupedGroup []boundRow
		for _, row := range group {
			ev2 := &evaluator{params: params, resolveCol: combinedResolver(row)}
			v, err := ev2.eval(f.Args[0])
			if err != nil || v.null {
				continue
			}
			if !seen[v.text] {
				seen[v.text] = true
				dedupedGroup = append(dedupedGroup, row)
			}
		}
		group = dedupedGroup
	}

	// Multi-arg or custom aggregates handled before the single-arg path.
	switch name {
	case "array_agg":
		if len(f.Args) < 1 {
			return value{null: true, oid: OIDTextArr}, nil
		}
		var elems []string
		for _, row := range group {
			ev2 := &evaluator{params: params, resolveCol: combinedResolver(row)}
			v, err := ev2.eval(f.Args[0])
			if err != nil || v.null {
				continue
			}
			elems = append(elems, v.text)
		}
		if len(elems) == 0 {
			return value{null: true, oid: OIDTextArr}, nil
		}
		return value{text: "{" + strings.Join(elems, ",") + "}", oid: OIDTextArr}, nil

	case "string_agg":
		if len(f.Args) < 2 {
			return value{null: true, oid: OIDText}, nil
		}
		sep := ""
		if len(group) > 0 {
			ev2 := &evaluator{params: params, resolveCol: combinedResolver(group[0])}
			sv, _ := ev2.eval(f.Args[1])
			sep = sv.text
		}
		var parts []string
		for _, row := range group {
			ev2 := &evaluator{params: params, resolveCol: combinedResolver(row)}
			v, err := ev2.eval(f.Args[0])
			if err != nil || v.null {
				continue
			}
			parts = append(parts, v.text)
		}
		if len(parts) == 0 {
			return value{null: true, oid: OIDText}, nil
		}
		return value{text: strings.Join(parts, sep), oid: OIDText}, nil

	case "json_agg", "jsonb_agg":
		if len(f.Args) < 1 {
			return value{text: "[]", oid: OIDJSONB}, nil
		}
		var elems []string
		for _, row := range group {
			ev2 := &evaluator{params: params, resolveCol: combinedResolver(row)}
			v, err := ev2.eval(f.Args[0])
			if err != nil {
				continue
			}
			if v.null {
				elems = append(elems, "null")
				continue
			}
			s := v.text
			if !strings.HasPrefix(s, "{") && !strings.HasPrefix(s, "[") && !strings.HasPrefix(s, `"`) {
				s = `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
			}
			elems = append(elems, s)
		}
		return value{text: "[" + strings.Join(elems, ",") + "]", oid: OIDJSONB}, nil

	case "bool_and", "every":
		if len(f.Args) < 1 {
			return boolValue(true), nil
		}
		for _, row := range group {
			ev2 := &evaluator{params: params, resolveCol: combinedResolver(row)}
			v, err := ev2.eval(f.Args[0])
			if err != nil || v.null || !asBool(v) {
				return boolValue(false), nil
			}
		}
		return boolValue(true), nil

	case "bool_or":
		if len(f.Args) < 1 {
			return boolValue(false), nil
		}
		for _, row := range group {
			ev2 := &evaluator{params: params, resolveCol: combinedResolver(row)}
			v, err := ev2.eval(f.Args[0])
			if err != nil || v.null {
				continue
			}
			if asBool(v) {
				return boolValue(true), nil
			}
		}
		return boolValue(false), nil

	case "json_object_agg", "jsonb_object_agg":
		if len(f.Args) < 2 {
			return value{text: "{}", oid: OIDJSONB}, nil
		}
		var b strings.Builder
		b.WriteString("{")
		first := true
		for _, row := range group {
			ev2 := &evaluator{params: params, resolveCol: combinedResolver(row)}
			k, err := ev2.eval(f.Args[0])
			if err != nil || k.null {
				continue
			}
			vv, err := ev2.eval(f.Args[1])
			if err != nil {
				continue
			}
			if !first {
				b.WriteString(",")
			}
			first = false
			b.WriteString(`"` + strings.ReplaceAll(k.text, `"`, `\"`) + `":`)
			if vv.null {
				b.WriteString("null")
			} else {
				b.WriteString(`"` + strings.ReplaceAll(vv.text, `"`, `\"`) + `"`)
			}
		}
		b.WriteString("}")
		return value{text: b.String(), oid: OIDJSONB}, nil
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

	case "percentile_cont", "percentile_disc":
		// percentile_cont(fraction) WITHIN GROUP (ORDER BY expr)
		if len(f.Args) < 1 {
			return value{null: true, oid: OIDFloat8}, nil
		}
		ev0 := &evaluator{params: params}
		fracVal, err := ev0.eval(f.Args[0])
		if err != nil {
			return value{null: true, oid: OIDFloat8}, nil
		}
		frac, _ := strconv.ParseFloat(fracVal.text, 64)

		var sortExpr ast.Node
		if f.WithinGroup != nil && len(f.WithinGroup.OrderBy) > 0 {
			sortExpr = f.WithinGroup.OrderBy[0].Node
		} else if len(f.Args) > 1 {
			sortExpr = f.Args[1]
		}

		var floatVals []float64
		for _, row := range group {
			ev2 := &evaluator{params: params, resolveCol: combinedResolver(row)}
			if sortExpr == nil {
				continue
			}
			v, e2 := ev2.eval(sortExpr)
			if e2 != nil || v.null {
				continue
			}
			fv, ferr := strconv.ParseFloat(v.text, 64)
			if ferr != nil {
				continue
			}
			floatVals = append(floatVals, fv)
		}

		if len(floatVals) == 0 {
			return value{null: true, oid: OIDFloat8}, nil
		}
		sort.Float64s(floatVals)

		if name == "percentile_disc" {
			idx := int(math.Ceil(frac*float64(len(floatVals)))) - 1
			if idx < 0 {
				idx = 0
			}
			if idx >= len(floatVals) {
				idx = len(floatVals) - 1
			}
			return value{text: strconv.FormatFloat(floatVals[idx], 'f', -1, 64), oid: OIDFloat8}, nil
		}
		// percentile_cont: linear interpolation
		pos := frac * float64(len(floatVals)-1)
		lo := int(pos)
		hi := lo + 1
		if hi >= len(floatVals) {
			hi = len(floatVals) - 1
		}
		interp := floatVals[lo] + (pos-float64(lo))*(floatVals[hi]-floatVals[lo])
		return value{text: strconv.FormatFloat(interp, 'f', -1, 64), oid: OIDFloat8}, nil

	case "mode":
		var sortExpr ast.Node
		if f.WithinGroup != nil && len(f.WithinGroup.OrderBy) > 0 {
			sortExpr = f.WithinGroup.OrderBy[0].Node
		} else if len(f.Args) > 0 {
			sortExpr = f.Args[0]
		}
		if sortExpr == nil {
			return value{null: true, oid: OIDText}, nil
		}
		counts := map[string]int{}
		for _, row := range group {
			ev2 := &evaluator{params: params, resolveCol: combinedResolver(row)}
			v, err := ev2.eval(sortExpr)
			if err != nil || v.null {
				continue
			}
			counts[v.text]++
		}
		bestCount, bestVal := 0, ""
		for val, cnt := range counts {
			if cnt > bestCount {
				bestCount = cnt
				bestVal = val
			}
		}
		if bestCount == 0 {
			return value{null: true, oid: OIDText}, nil
		}
		return value{text: bestVal, oid: OIDText}, nil

	case "first", "first_value":
		if len(f.Args) < 1 {
			return value{null: true, oid: OIDText}, nil
		}
		for _, row := range group {
			ev2 := &evaluator{params: params, resolveCol: combinedResolver(row)}
			v, err := ev2.eval(f.Args[0])
			if err != nil || v.null {
				continue
			}
			return v, nil
		}
		return value{null: true, oid: OIDText}, nil

	case "last", "last_value":
		if len(f.Args) < 1 {
			return value{null: true, oid: OIDText}, nil
		}
		var last value
		hasVal := false
		for _, row := range group {
			ev2 := &evaluator{params: params, resolveCol: combinedResolver(row)}
			v, err := ev2.eval(f.Args[0])
			if err != nil || v.null {
				continue
			}
			last = v
			hasVal = true
		}
		if hasVal {
			return last, nil
		}
		return value{null: true, oid: OIDText}, nil
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
