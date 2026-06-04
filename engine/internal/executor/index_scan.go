package executor

import (
	"bytes"
	"context"
	"sort"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

func eqFold(a, b string) bool { return strings.EqualFold(a, b) }

// sortRows orders rows in place per the ORDER BY clause, using memcomparable
// encoding of each sort key (ADR-022) so typed columns sort correctly (e.g.
// integers numerically, not lexicographically). NULLs sort last under ASC and
// first under DESC, matching PostgreSQL defaults. Stable to keep ties in their
// input order.
func (e *execImpl) sortRows(rows []boundRow, clause []*ast.SortBy, params []Datum) error {
	type item struct {
		row   boundRow
		keys  [][]byte
		nulls []bool
	}
	items := make([]item, len(rows))
	for i, row := range rows {
		ev := &evaluator{params: params, resolveCol: combinedResolver(row)}
		it := item{row: row, keys: make([][]byte, len(clause)), nulls: make([]bool, len(clause))}
		for j, sb := range clause {
			v, err := ev.eval(sb.Node)
			if err != nil {
				return err
			}
			if v.null {
				it.nulls[j] = true
				continue
			}
			enc, err := orderEncode(v.oid, v.text)
			if err != nil {
				return err
			}
			it.keys[j] = enc
		}
		items[i] = it
	}

	sort.SliceStable(items, func(a, b int) bool {
		for j, sb := range clause {
			ia, ib := items[a], items[b]
			c := compareSortKey(ia.nulls[j], ia.keys[j], ib.nulls[j], ib.keys[j])
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
	for i := range items {
		rows[i] = items[i].row
	}
	return nil
}

// compareSortKey orders two encoded keys with NULLs treated as greater than any
// value (so ASC puts them last; DESC, which reverses, puts them first).
func compareSortKey(aNull bool, a []byte, bNull bool, b []byte) int {
	switch {
	case aNull && bNull:
		return 0
	case aNull:
		return 1
	case bNull:
		return -1
	default:
		return bytes.Compare(a, b)
	}
}

// indexScan is the result of the index planner: candidate rows for a
// single-table SELECT, plus whether they are already in the order requested by
// the query's ORDER BY (so the caller can skip the in-memory sort).
type indexScan struct {
	rows    []boundRow
	ordered bool
}

// colRef is a parsed column reference (optional table/alias qualifier + name).
type colRef struct {
	qualifier string
	name      string
}

// colBounds is a lower/upper bound on one column derived from WHERE. lo/hi are
// the constant-side text values; the strictness is handled downstream by the
// re-applied WHERE predicate, so the scan only needs the (inclusive) endpoints.
type colBounds struct {
	col    string
	hasLo  bool
	loText string
	hasHi  bool
	hiText string
}

// planIndexScan decides whether a single-table SELECT can be served by a
// secondary index (equality, range, and/or ORDER BY) instead of a full scan.
// Returns nil to fall back to a full scan. main-branch only in V0.3
// (feature-branch index COW fall-through is deferred — see ADR-022).
func (e *execImpl) planIndexScan(ctx context.Context, txn *transactions.Txn, sess *session.Session, s *ast.SelectStmt, params []Datum) (*indexScan, error) {
	if sess.Branch() != "main" || len(s.FromClause) != 1 {
		return nil, nil
	}
	rv, ok := s.FromClause[0].(*ast.RangeVar)
	if !ok || rv.RelName == OTelSpansTable {
		return nil, nil
	}
	// A CTE reference is not a stored table — let materialize serve it.
	if _, isCTE := lookupCTE(ctx, rv.RelName); isCTE {
		return nil, nil
	}
	alias := rv.RelName
	if rv.Alias != nil && rv.Alias.AliasName != "" {
		alias = rv.Alias.AliasName
	}
	matchesRel := func(q string) bool {
		return q == "" || eqFold(q, alias) || eqFold(q, rv.RelName)
	}

	sch, err := e.resolveSchema(ctx, txn, sess, rv.RelName)
	if err != nil {
		return nil, nil // let the full-scan path surface the error
	}

	defs, err := e.loadIndexes(ctx, txn, sess, rv.RelName)
	if err != nil {
		return nil, err
	}

	// Expression-index exact lookup: if WHERE has `<expr> = const` where <expr>
	// structurally matches an expression index's stored expression, serve it via a
	// tuple-keyed scan over that index. Checked before the column paths so a
	// functional index (e.g. on lower(email)) is preferred for matching predicates.
	if s.SortClause == nil {
		if scan, used, err := e.tryExprEquality(ctx, txn, sess, rv.RelName, alias, sch, defs, s.WhereClause, matchesRel, params); used || err != nil {
			return scan, err
		}
	}

	// Composite-index exact lookup: if WHERE has equality on every column of a
	// multi-column index, do one tuple-keyed scan.
	if s.SortClause == nil {
		if scan, used, err := e.tryCompositeEquality(ctx, txn, sess, rv.RelName, alias, sch, defs, s.WhereClause, matchesRel, params); used || err != nil {
			return scan, err
		}
	}

	// Partial single-column equality: WHERE `... AND col = const` where a partial
	// index on col exists and the query predicate implies the partial predicate.
	// This handles the canonical partial-index case (e.g. a partial index
	// `WHERE active` serving `WHERE active AND email = 'x'`), which the
	// single-column range planner below misses because the extra boolean conjunct
	// prevents single-column bound extraction.
	if s.SortClause == nil && s.WhereClause != nil {
		if scan, used, err := e.trySingleColEquality(ctx, txn, sess, rv.RelName, alias, sch, defs, s.WhereClause, matchesRel, params); used || err != nil {
			return scan, err
		}
	}

	// Bounds from WHERE (a single comparison, or AND of comparisons on one col).
	bounds, boundsOK := extractColBounds(s.WhereClause, matchesRel, params)

	// Order direction from ORDER BY (single key on a column).
	orderCol, desc, orderOK := orderByCol(s.SortClause, matchesRel)

	// Pick the driving column: the WHERE column, else the ORDER BY column. If
	// both are present they must be the same column for the index to serve both.
	var driveCol string
	switch {
	case boundsOK && orderOK:
		if !eqFold(bounds.col, orderCol) {
			// WHERE on one indexed col, ORDER BY on another: serve the WHERE via
			// index (range), and let the caller sort in memory.
			driveCol = bounds.col
			orderOK = false
		} else {
			driveCol = bounds.col
		}
	case boundsOK:
		driveCol = bounds.col
	case orderOK:
		driveCol = orderCol
	default:
		return nil, nil
	}

	ci := sch.colIndex(driveCol)
	if ci < 0 {
		return nil, nil
	}

	// PK equality is the cheapest path: a single point-get, no index needed.
	if boundsOK && !orderOK && sch.PKIndex == ci && bounds.isEquality() {
		row, err := e.pkPointGet(ctx, txn, sess, rv.RelName, alias, sch, bounds.loText)
		if err != nil {
			return nil, err
		}
		return &indexScan{rows: row, ordered: true}, nil
	}

	// A partial index may serve this scan only when the query predicate implies
	// its predicate (so it omits no qualifying row). usePartial is nil when the
	// scan is ORDER BY-only with no WHERE, where a partial index could drop rows.
	var usePartial func(string) bool
	if s.WhereClause != nil {
		where := s.WhereClause
		usePartial = func(p string) bool { return predicateImplies(where, p) }
	}
	def, ok := findIndexOn(defs, driveCol, usePartial)
	if !ok {
		return nil, nil
	}

	colType := sch.Cols[ci].TypeOID
	enc := e.store.Encoder()
	colPrefix := enc.IndexColumnPrefix(sess.Namespace(), sess.Branch(), def.Table, def.Name).Bytes()

	// Encode the scan endpoints. startKey/stopKey are inclusive byte bounds;
	// predicate strictness (> vs >=, < vs <=) is re-checked downstream.
	var loKey, hiKey []byte
	if boundsOK && bounds.hasLo {
		ev, err := orderEncode(colType, bounds.loText)
		if err != nil {
			return nil, err
		}
		loKey = append(append([]byte(nil), colPrefix...), ev...)
	}
	if boundsOK && bounds.hasHi {
		ev, err := orderEncode(colType, bounds.hiText)
		if err != nil {
			return nil, err
		}
		// 0xFF sentinel so all pk entries of the hi value are included.
		hiKey = append(append(append([]byte(nil), colPrefix...), ev...), 0xFF)
	}

	// Early-stop at LIMIT only for a pure ORDER BY ... LIMIT (no WHERE): then
	// every scanned row is in the result, so stopping after n is correct. With a
	// WHERE present, strict-bound rows may be filtered downstream, so we must
	// scan the full range and let execSelectFrom apply LIMIT after filtering.
	limit := -1
	if orderOK && s.WhereClause == nil && s.LimitCount != nil && !s.Distinct {
		n, err := evalIntLimit(s.LimitCount, params)
		if err != nil {
			return nil, err
		}
		limit = n
	}

	rows, err := e.scanIndexRange(ctx, txn, sess, rv.RelName, alias, sch, colPrefix, loKey, hiKey, desc, limit)
	if err != nil {
		return nil, err
	}
	return &indexScan{rows: rows, ordered: orderOK}, nil
}

// tryCompositeEquality serves a SELECT whose WHERE has equality on every column
// of a multi-column index via one tuple-keyed scan. Returns used=false to fall
// through to the single-column planner.
func (e *execImpl) tryCompositeEquality(ctx context.Context, txn *transactions.Txn, sess *session.Session, table, alias string, sch *tableSchema, defs []indexDef, where ast.Node, matchesRel func(string) bool, params []Datum) (*indexScan, bool, error) {
	eq := collectEqualities(where, matchesRel, params)
	if len(eq) < 2 {
		return nil, false, nil
	}
	eqSet := map[string]bool{}
	for c := range eq {
		eqSet[c] = true
	}
	var usePartial func(string) bool
	if where != nil {
		w := where
		usePartial = func(p string) bool { return predicateImplies(w, p) }
	}
	def, ok := findIndexForEquality(defs, eqSet, usePartial)
	if !ok || len(def.Columns) < 2 {
		return nil, false, nil
	}
	// Build the tuple from the equality constants in index-column order.
	var tuple []byte
	for _, col := range def.Columns {
		text := eq[strings.ToLower(col)]
		part, err := orderEncode(sch.Cols[sch.colIndex(col)].TypeOID, text)
		if err != nil {
			return nil, false, err
		}
		tuple = append(tuple, part...)
	}
	enc := e.store.Encoder()
	colPrefix := enc.IndexColumnPrefix(sess.Namespace(), sess.Branch(), def.Table, def.Name).Bytes()
	loKey := append(append([]byte(nil), colPrefix...), tuple...)
	hiKey := append(append([]byte(nil), loKey...), 0xFF)
	rows, err := e.scanIndexRange(ctx, txn, sess, table, alias, sch, colPrefix, loKey, hiKey, false, -1)
	if err != nil {
		return nil, false, err
	}
	return &indexScan{rows: rows, ordered: false}, true, nil
}

// tryExprEquality serves a SELECT whose WHERE contains `<expr> = const` (or
// `const = <expr>`) where <expr> structurally matches an expression index's
// stored expression. It scans the matching index for the encoded constant. The
// full WHERE is re-applied downstream, so an over-broad match is still correct;
// here we only need to ensure the index is genuinely keyed on <expr>.
//
// Partial expression indexes are required to have their predicate implied by the
// query predicate before use (conservative); a plain expression index is used
// whenever its expression matches.
func (e *execImpl) tryExprEquality(ctx context.Context, txn *transactions.Txn, sess *session.Session, table, alias string, sch *tableSchema, defs []indexDef, where ast.Node, matchesRel func(string) bool, params []Datum) (*indexScan, bool, error) {
	// Gather every top-level `<non-column-expr> = const` from the WHERE clause.
	cands := collectExprEqualities(where)
	if len(cands) == 0 {
		return nil, false, nil
	}
	for _, cand := range cands {
		for _, d := range defs {
			if d.Where != "" {
				// Partial expression index: only usable when Q ⇒ P.
				if !predicateImplies(where, d.Where) {
					continue
				}
			}
			if !exprMatchesIndex(d, cand.expr) {
				continue
			}
			text, ok := resolveConstText(cand.constNode, params)
			if !ok {
				continue // unbound/NULL param or non-constant: cannot key the index
			}
			// Determine the OID the index used to encode this element. Re-evaluate
			// the stored expression's type by evaluating against a NULL-free probe is
			// overkill; instead encode as text (the index encoder uses the expr's
			// evaluated OID, defaulting to text). We mirror that default.
			part, eerr := orderEncode(OIDText, text)
			if eerr != nil {
				continue
			}
			enc := e.store.Encoder()
			colPrefix := enc.IndexColumnPrefix(sess.Namespace(), sess.Branch(), d.Table, d.Name).Bytes()
			loKey := append(append([]byte(nil), colPrefix...), part...)
			hiKey := append(append([]byte(nil), loKey...), 0xFF)
			rows, err := e.scanIndexRange(ctx, txn, sess, table, alias, sch, colPrefix, loKey, hiKey, false, -1)
			if err != nil {
				return nil, false, err
			}
			return &indexScan{rows: rows, ordered: false}, true, nil
		}
	}
	return nil, false, nil
}

// exprEqCand is one `<expr> = const` candidate from a WHERE clause.
type exprEqCand struct {
	expr      ast.Node
	constNode ast.Node
}

// collectExprEqualities gathers `<expr> = const` comparisons where <expr> is not
// a plain column reference (those are handled by the column planner). Descends
// only top-level AND conjuncts.
func collectExprEqualities(where ast.Node) []exprEqCand {
	var out []exprEqCand
	var walk func(n ast.Node)
	walk = func(n ast.Node) {
		switch e := n.(type) {
		case *ast.BoolExpr:
			if e.Op == ast.AND_EXPR {
				for _, a := range e.Args {
					walk(a)
				}
			}
		case *ast.A_Expr:
			if e.Kind != ast.AEXPR_OP || e.Name != "=" {
				return
			}
			// Identify the non-constant side as the expression and the constant side.
			if isConstNode(e.Rexpr) && isIndexableExpr(e.Lexpr) {
				out = append(out, exprEqCand{expr: e.Lexpr, constNode: e.Rexpr})
			} else if isConstNode(e.Lexpr) && isIndexableExpr(e.Rexpr) {
				out = append(out, exprEqCand{expr: e.Rexpr, constNode: e.Lexpr})
			}
		}
	}
	walk(where)
	return out
}

// isIndexableExpr reports whether n is a non-trivial expression worth matching
// against an expression index (i.e. not a bare column reference or constant).
func isIndexableExpr(n ast.Node) bool {
	switch n.(type) {
	case *ast.FuncCall, *ast.A_Expr, *ast.TypeCast, *ast.CaseExpr:
		return true
	}
	return false
}

// trySingleColEquality serves a SELECT whose WHERE contains `col = const` (amid
// other conjuncts) using an index on col. It exists primarily to use a partial
// index whose predicate the query implies; it considers full indexes too, which
// is always correct (the full WHERE is re-applied downstream). Returns used=false
// when no single-column equality has a usable index.
func (e *execImpl) trySingleColEquality(ctx context.Context, txn *transactions.Txn, sess *session.Session, table, alias string, sch *tableSchema, defs []indexDef, where ast.Node, matchesRel func(string) bool, params []Datum) (*indexScan, bool, error) {
	eq := collectEqualities(where, matchesRel, params)
	if len(eq) == 0 {
		return nil, false, nil
	}
	usePartial := func(p string) bool { return predicateImplies(where, p) }
	for col, text := range eq {
		ci := sch.colIndex(col)
		if ci < 0 {
			continue
		}
		// Prefer a partial index implied by the query; fall back to a full
		// single-column index on this column.
		var chosen indexDef
		ok := false
		for _, d := range defs {
			if d.isExpr() || len(d.Columns) != 1 || !strings.EqualFold(d.Columns[0], col) {
				continue
			}
			if d.Where != "" {
				if !usePartial(d.Where) {
					continue
				}
				chosen, ok = d, true // partial-and-implied: best (smallest scan)
				break
			}
			if !ok {
				chosen, ok = d, true
			}
		}
		if !ok {
			continue
		}
		part, err := orderEncode(sch.Cols[ci].TypeOID, text)
		if err != nil {
			return nil, false, err
		}
		enc := e.store.Encoder()
		colPrefix := enc.IndexColumnPrefix(sess.Namespace(), sess.Branch(), chosen.Table, chosen.Name).Bytes()
		loKey := append(append([]byte(nil), colPrefix...), part...)
		hiKey := append(append([]byte(nil), loKey...), 0xFF)
		rows, rerr := e.scanIndexRange(ctx, txn, sess, table, alias, sch, colPrefix, loKey, hiKey, false, -1)
		if rerr != nil {
			return nil, false, rerr
		}
		return &indexScan{rows: rows, ordered: false}, true, nil
	}
	return nil, false, nil
}

// collectEqualities gathers all `col = const` equalities (lower-cased column →
// constant text) from a WHERE clause that is a comparison or an AND of them.
func collectEqualities(where ast.Node, matchesRel func(string) bool, params []Datum) map[string]string {
	out := map[string]string{}
	var walk func(n ast.Node)
	walk = func(n ast.Node) {
		switch e := n.(type) {
		case *ast.BoolExpr:
			if e.Op == ast.AND_EXPR {
				for _, a := range e.Args {
					walk(a)
				}
			}
		case *ast.A_Expr:
			b, ok := boundFromComparisonP(e, matchesRel, params)
			if ok && b.hasLo && b.hasHi && b.loText == b.hiText {
				out[strings.ToLower(b.col)] = b.loText
			}
		}
	}
	walk(where)
	return out
}

// scanIndexRange iterates the index column between loKey and hiKey (inclusive
// byte bounds; nil means unbounded that side), forward or reverse, fetching each
// row by the pk stored in the index entry's value.
func (e *execImpl) scanIndexRange(ctx context.Context, txn *transactions.Txn, sess *session.Session, table, alias string, sch *tableSchema, colPrefix, loKey, hiKey []byte, desc bool, limit int) ([]boundRow, error) {
	enc := e.store.Encoder()
	prefixKey := storage.RawKey(colPrefix)

	var it storage.Iterator
	if desc {
		it = e.txn.NewReverseIterator(txn, prefixKey)
	} else {
		it = e.txn.NewIterator(txn, prefixKey)
	}
	defer it.Close()

	// Position at the starting edge.
	if desc {
		if hiKey != nil {
			it.Seek(storage.RawKey(hiKey))
		} else {
			it.Rewind()
		}
	} else {
		if loKey != nil {
			it.Seek(storage.RawKey(loKey))
		} else {
			it.Rewind()
		}
	}

	var out []boundRow
	for ; it.Valid(); it.Next() {
		key := it.Key().Bytes()
		if desc {
			if loKey != nil && bytes.Compare(key, loKey) < 0 {
				break
			}
		} else {
			if hiKey != nil && bytes.Compare(key, hiKey) > 0 {
				break
			}
		}
		pk, err := it.Value()
		if err != nil {
			return nil, err
		}
		rowKey := enc.RowKey(sess.Namespace(), sess.Branch(), table, pk)
		raw, err := e.txn.Get(ctx, txn, rowKey)
		if err != nil {
			continue // index entry without a live row; skip
		}
		if storage.IsTombstone(raw) {
			continue
		}
		cells, err := decodeRow(raw, len(sch.Cols))
		if err != nil {
			return nil, err
		}
		out = append(out, boundRow{{alias: alias, schema: sch, cells: cells}})
		if limit >= 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// pkPointGet fetches a single row by primary key (the cheapest equality path).
func (e *execImpl) pkPointGet(ctx context.Context, txn *transactions.Txn, sess *session.Session, table, alias string, sch *tableSchema, pkText string) ([]boundRow, error) {
	rowKey := e.store.Encoder().RowKey(sess.Namespace(), sess.Branch(), table, []byte(pkText))
	raw, err := e.txn.Get(ctx, txn, rowKey)
	if err != nil || storage.IsTombstone(raw) {
		return []boundRow{}, nil
	}
	cells, err := decodeRow(raw, len(sch.Cols))
	if err != nil {
		return nil, err
	}
	return []boundRow{{{alias: alias, schema: sch, cells: cells}}}, nil
}

func (b colBounds) isEquality() bool {
	return b.hasLo && b.hasHi && b.loText == b.hiText
}

// extractColBounds derives bounds on a single column from a WHERE clause that is
// a comparison (=, >, >=, <, <=) or an AND of such comparisons on one column.
func extractColBounds(where ast.Node, matchesRel func(string) bool, params []Datum) (colBounds, bool) {
	if where == nil {
		return colBounds{}, false
	}
	switch n := where.(type) {
	case *ast.A_Expr:
		return boundFromComparisonP(n, matchesRel, params)
	case *ast.BoolExpr:
		if n.Op != ast.AND_EXPR {
			return colBounds{}, false
		}
		var acc colBounds
		have := false
		for _, arg := range n.Args {
			b, ok := extractColBounds(arg, matchesRel, params)
			if !ok {
				return colBounds{}, false
			}
			if !have {
				acc, have = b, true
				continue
			}
			if !eqFold(acc.col, b.col) {
				return colBounds{}, false // AND across different columns
			}
			acc = mergeBounds(acc, b)
		}
		return acc, have
	}
	return colBounds{}, false
}

// boundFromComparisonP turns one `col OP const` (or `const OP col`) into bounds,
// resolving constants from A_Const literals or bound parameters: a ParamRef
// RHS/LHS ($N) resolves to its bound value in params, so `col = $1` (or a range
// `col > $1`) drives index acceleration just like a literal. An unbound/NULL
// param is not resolvable and yields ok=false (seq-scan fallback).
func boundFromComparisonP(ae *ast.A_Expr, matchesRel func(string) bool, params []Datum) (colBounds, bool) {
	if ae.Kind != ast.AEXPR_OP {
		return colBounds{}, false
	}
	op := ae.Name
	var c colRef
	var constNode ast.Node
	var flipped bool
	if cr, ok := asColRef(ae.Lexpr); ok && isConstNode(ae.Rexpr) {
		c, constNode = cr, ae.Rexpr
	} else if cr, ok := asColRef(ae.Rexpr); ok && isConstNode(ae.Lexpr) {
		c, constNode, flipped = cr, ae.Lexpr, true
	} else {
		return colBounds{}, false
	}
	if !matchesRel(c.qualifier) {
		return colBounds{}, false
	}
	text, ok := resolveConstText(constNode, params)
	if !ok {
		return colBounds{}, false
	}
	// Normalise so the operator is read as `col OP const`.
	if flipped {
		op = flipOp(op)
	}
	b := colBounds{col: c.name}
	switch op {
	case "=":
		b.hasLo, b.loText, b.hasHi, b.hiText = true, text, true, text
	case ">", ">=":
		b.hasLo, b.loText = true, text
	case "<", "<=":
		b.hasHi, b.hiText = true, text
	default:
		return colBounds{}, false
	}
	return b, true
}

func mergeBounds(a, b colBounds) colBounds {
	out := colBounds{col: a.col}
	if a.hasLo {
		out.hasLo, out.loText = true, a.loText
	}
	if b.hasLo {
		out.hasLo, out.loText = true, b.loText
	}
	if a.hasHi {
		out.hasHi, out.hiText = true, a.hiText
	}
	if b.hasHi {
		out.hasHi, out.hiText = true, b.hiText
	}
	return out
}

func flipOp(op string) string {
	switch op {
	case ">":
		return "<"
	case ">=":
		return "<="
	case "<":
		return ">"
	case "<=":
		return ">="
	default:
		return op // = stays =
	}
}

// orderByCol returns the single ORDER BY column and direction, if the ORDER BY
// is one key on a (possibly qualified) column reference.
func orderByCol(sort []*ast.SortBy, matchesRel func(string) bool) (col string, desc bool, ok bool) {
	if len(sort) != 1 {
		return "", false, false
	}
	c, ok := asColRef(sort[0].Node)
	if !ok || !matchesRel(c.qualifier) {
		return "", false, false
	}
	return c.name, sort[0].SortDir == 2, true // SortDir 2 == DESC
}

func asColRef(n ast.Node) (colRef, bool) {
	cr, ok := n.(*ast.ColumnRef)
	if !ok || len(cr.Fields) == 0 {
		return colRef{}, false
	}
	last := cr.Fields[len(cr.Fields)-1]
	if last == "*" {
		return colRef{}, false
	}
	c := colRef{name: last}
	if len(cr.Fields) > 1 {
		c.qualifier = cr.Fields[len(cr.Fields)-2]
	}
	return c, true
}

func isConstNode(n ast.Node) bool {
	switch n.(type) {
	case *ast.A_Const, *ast.ParamRef:
		return true
	}
	return false
}

// constText returns the literal text of a constant node (A_Const only; ParamRef
// is not resolvable here without params, so bounds skip it — the full WHERE
// re-check still applies, just without index acceleration).
func constText(n ast.Node) (string, bool) {
	if c, ok := n.(*ast.A_Const); ok {
		return c.Val, true
	}
	return "", false
}

// resolveConstText returns the constant text of a node resolvable at scan time:
// an A_Const literal, or a ParamRef ($N) whose bound value in params is a known
// non-NULL constant. This lets `<expr> = $1` and `col = $1` drive an
// expression/partial index exactly as a literal would. A NULL-bound or
// out-of-range parameter is not resolvable (ok=false) — the caller then falls
// back to a sequential scan (still correct; the full WHERE re-check applies).
func resolveConstText(n ast.Node, params []Datum) (string, bool) {
	switch c := n.(type) {
	case *ast.A_Const:
		if c.Type == ast.ConstNull {
			return "", false
		}
		return c.Val, true
	case *ast.ParamRef:
		idx := c.Number - 1
		if idx < 0 || idx >= len(params) {
			return "", false
		}
		p := params[idx]
		if p.Null {
			return "", false
		}
		return p.Text, true
	}
	return "", false
}
