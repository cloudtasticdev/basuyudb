package executor

import (
	"sort"
	"strconv"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
)

// evalWindowRef returns the precomputed value of a window function call for the
// current output row. It is only valid during windowed projection.
func (ev *evaluator) evalWindowRef(f *ast.FuncCall) (value, error) {
	if ev.windowVals == nil {
		return value{}, newExecError("42P20", "window functions are not allowed in this context")
	}
	vals, ok := ev.windowVals[f]
	if !ok || ev.rowIdx < 0 || ev.rowIdx >= len(vals) {
		return value{null: true, oid: OIDUnknown}, nil
	}
	return vals[ev.rowIdx], nil
}

// collectWindowFuncs returns every distinct window function call in the target
// list (a FuncCall with a non-nil OVER clause).
func collectWindowFuncs(targets []*ast.ResTarget) []*ast.FuncCall {
	var out []*ast.FuncCall
	seen := map[*ast.FuncCall]bool{}
	for _, t := range targets {
		_ = ast.Walk(t.Val, func(n ast.Node) error {
			if fc, ok := n.(*ast.FuncCall); ok && fc.Over != nil && !seen[fc] {
				seen[fc] = true
				out = append(out, fc)
			}
			return nil
		})
	}
	return out
}

// hasWindowFunc reports whether any target-list expression is a window function.
func hasWindowFunc(targets []*ast.ResTarget) bool {
	return len(collectWindowFuncs(targets)) > 0
}

// computeWindows evaluates every window function over the (already WHERE-filtered)
// rows, returning per-row values keyed by the function-call node.
func (e *execImpl) computeWindows(rows []boundRow, targets []*ast.ResTarget, params []Datum) (map[ast.Node][]value, error) {
	funcs := collectWindowFuncs(targets)
	if len(funcs) == 0 {
		return nil, nil
	}
	out := make(map[ast.Node][]value, len(funcs))
	for _, f := range funcs {
		vals, err := computeOneWindow(rows, f, params)
		if err != nil {
			return nil, err
		}
		out[f] = vals
	}
	return out, nil
}

// computeOneWindow computes a single window function's value for every row.
func computeOneWindow(rows []boundRow, f *ast.FuncCall, params []Datum) ([]value, error) {
	n := len(rows)
	vals := make([]value, n)
	name := strings.ToLower(strings.Join(f.FuncName, "."))

	// Partition each row.
	partKey := make([]string, n)
	for i, row := range rows {
		ev := &evaluator{params: params, resolveCol: combinedResolver(row)}
		var b strings.Builder
		for _, pe := range f.Over.PartitionBy {
			v, err := ev.eval(pe)
			if err != nil {
				return nil, err
			}
			if v.null {
				b.WriteString("\x00|")
			} else {
				b.WriteByte(1)
				b.WriteString(v.text)
				b.WriteByte('|')
			}
		}
		partKey[i] = b.String()
	}

	// Group indices by partition, preserving first-seen partition order.
	partitions := map[string][]int{}
	for i := 0; i < n; i++ {
		partitions[partKey[i]] = append(partitions[partKey[i]], i)
	}

	for _, idxs := range partitions {
		// Order within the partition.
		ob := f.Over.OrderBy
		if len(ob) > 0 {
			sort.SliceStable(idxs, func(a, b int) bool {
				return compareOrderKeys(rows[idxs[a]], rows[idxs[b]], ob, params) < 0
			})
		}
		if err := computePartition(name, f, rows, idxs, vals, params); err != nil {
			return nil, err
		}
	}
	return vals, nil
}

// computePartition writes the window value for every row index in one ordered
// partition. Ranking functions use the ORDER BY peer structure; aggregates use a
// RANGE frame (whole partition when no ORDER BY, else running through peers).
func computePartition(name string, f *ast.FuncCall, rows []boundRow, idxs []int, vals []value, params []Datum) error {
	ob := f.Over.OrderBy

	switch name {
	case "row_number":
		for pos, ri := range idxs {
			vals[ri] = value{text: strconv.Itoa(pos + 1), oid: OIDInt8}
		}
		return nil
	case "rank", "dense_rank":
		rank, dense := 0, 0
		for pos, ri := range idxs {
			newPeer := pos == 0 || compareOrderKeys(rows[idxs[pos-1]], rows[ri], ob, params) != 0
			if newPeer {
				dense++
				rank = pos + 1
			}
			if name == "rank" {
				vals[ri] = value{text: strconv.Itoa(rank), oid: OIDInt8}
			} else {
				vals[ri] = value{text: strconv.Itoa(dense), oid: OIDInt8}
			}
		}
		return nil
	}

	if !isAggregateName(name) {
		return newExecError("0A000", "window function %q is not supported", name)
	}

	// Aggregate window: evaluate the argument per row, then either total the whole
	// partition (no ORDER BY) or accumulate a running RANGE frame (with ORDER BY).
	args := make([]value, len(idxs))
	for pos, ri := range idxs {
		if f.AggStar {
			continue
		}
		ev := &evaluator{params: params, resolveCol: combinedResolver(rows[ri])}
		v, err := ev.eval(f.Args[0])
		if err != nil {
			return err
		}
		args[pos] = v
	}

	if len(ob) == 0 {
		whole := aggregateOver(name, f, args, 0, len(idxs))
		for _, ri := range idxs {
			vals[ri] = whole
		}
		return nil
	}
	// Running frame: include all peers up to the current row (RANGE semantics).
	pos := 0
	for pos < len(idxs) {
		end := pos + 1
		for end < len(idxs) && compareOrderKeys(rows[idxs[end-1]], rows[idxs[end]], ob, params) == 0 {
			end++
		}
		v := aggregateOver(name, f, args, 0, end)
		for p := pos; p < end; p++ {
			vals[idxs[p]] = v
		}
		pos = end
	}
	return nil
}

// aggregateOver reduces a window aggregate over args[lo:hi].
func aggregateOver(name string, f *ast.FuncCall, args []value, lo, hi int) value {
	switch name {
	case "count":
		if f.AggStar {
			return value{text: strconv.Itoa(hi - lo), oid: OIDInt8}
		}
		c := 0
		for i := lo; i < hi; i++ {
			if !args[i].null {
				c++
			}
		}
		return value{text: strconv.Itoa(c), oid: OIDInt8}
	case "sum", "avg":
		var sum float64
		cnt := 0
		for i := lo; i < hi; i++ {
			if args[i].null {
				continue
			}
			x, err := strconv.ParseFloat(args[i].text, 64)
			if err != nil {
				continue
			}
			sum += x
			cnt++
		}
		if cnt == 0 {
			return value{null: true, oid: OIDNumeric}
		}
		if name == "avg" {
			return value{text: strconv.FormatFloat(sum/float64(cnt), 'g', -1, 64), oid: OIDNumeric}
		}
		return value{text: trimFloat(sum), oid: OIDNumeric}
	case "min", "max":
		var best value
		have := false
		for i := lo; i < hi; i++ {
			if args[i].null {
				continue
			}
			if !have {
				best, have = args[i], true
				continue
			}
			c := compareDatum(Datum{Text: args[i].text}, Datum{Text: best.text})
			if (name == "min" && c < 0) || (name == "max" && c > 0) {
				best = args[i]
			}
		}
		if !have {
			return value{null: true, oid: OIDUnknown}
		}
		return best
	}
	return value{null: true, oid: OIDUnknown}
}

// compareOrderKeys compares two rows by a window ORDER BY list (with ASC/DESC).
func compareOrderKeys(a, b boundRow, ob []*ast.SortBy, params []Datum) int {
	for _, sb := range ob {
		ea := &evaluator{params: params, resolveCol: combinedResolver(a)}
		eb := &evaluator{params: params, resolveCol: combinedResolver(b)}
		va, _ := ea.eval(sb.Node)
		vb, _ := eb.eval(sb.Node)
		c := compareDatum(Datum{Null: va.null, Text: va.text}, Datum{Null: vb.null, Text: vb.text})
		if c != 0 {
			if sb.SortDir == 2 { // DESC
				return -c
			}
			return c
		}
	}
	return 0
}

// trimFloat formats a float without a trailing ".0" for whole numbers.
func trimFloat(x float64) string {
	if x == float64(int64(x)) {
		return strconv.FormatInt(int64(x), 10)
	}
	return strconv.FormatFloat(x, 'g', -1, 64)
}
