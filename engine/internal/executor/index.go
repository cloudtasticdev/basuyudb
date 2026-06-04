package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// indexDef is a persisted secondary-index definition. Columns holds one or more
// indexed columns (composite). Index entries are keyed under the index name and
// carry the concatenated memcomparable encoding of the columns (ADR-022).
type indexDef struct {
	Name    string   `json:"name"`
	Table   string   `json:"table"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique"`
	// Exprs, when non-empty, is parallel to the index elements: a non-empty entry
	// holds the deparsed expression for an expression index element, "" for a
	// plain column element (whose name is in Columns at the same position). When
	// any entry is non-empty the index is an expression index and its key is the
	// concatenation of per-element encodings (column value or evaluated expr).
	Exprs []string `json:"exprs,omitempty"`
	// Where is the deparsed partial-index predicate; only rows satisfying it are
	// indexed. Empty for a full index.
	Where string `json:"where,omitempty"`
}

// isExpr reports whether the index has any expression element.
func (d indexDef) isExpr() bool {
	for _, e := range d.Exprs {
		if e != "" {
			return true
		}
	}
	return false
}

// hasColumn reports whether the index covers the named column.
func (d indexDef) hasColumn(col string) bool {
	for _, c := range d.Columns {
		if strings.EqualFold(c, col) {
			return true
		}
	}
	return false
}

// encodeIndexTuple concatenates the memcomparable encodings of an index's
// columns for a row. Each per-type encoding is prefix-free, so the concatenation
// orders correctly as a tuple. Returns ok=false if any indexed column is NULL
// (NULLs are not indexed).
func encodeIndexTuple(sch *tableSchema, columns []string, cells []Datum) (enc []byte, ok bool, err error) {
	for _, col := range columns {
		ci := sch.colIndex(col)
		if ci < 0 || cells[ci].Null {
			return nil, false, nil
		}
		part, e := orderEncode(sch.Cols[ci].TypeOID, cells[ci].Text)
		if e != nil {
			return nil, false, e
		}
		enc = append(enc, part...)
	}
	return enc, true, nil
}

// indexListKey stores the JSON array of a table's index definitions under the
// table's schema metadata namespace: /ns/{ns}/meta/schema/{table}#idx
func (e *execImpl) indexListKey(sess *session.Session, table string) storage.Key {
	return e.store.Encoder().SchemaKey(sess.Namespace(), table+"#idx")
}

// loadIndexes returns a table's index definitions (empty if none).
func (e *execImpl) loadIndexes(ctx context.Context, txn *transactions.Txn, sess *session.Session, table string) ([]indexDef, error) {
	raw, err := e.txn.Get(ctx, txn, e.indexListKey(sess, table))
	if errors.Is(err, storage.ErrKeyNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var defs []indexDef
	if err := json.Unmarshal(raw, &defs); err != nil {
		return nil, newExecError("XX000", "corrupt index metadata for %q: %v", table, err)
	}
	return defs, nil
}

// findIndexOn returns an index whose LEADING column is col (usable for a
// single-column range or ORDER BY on col), preferring a single-column index.
//
// usePartial reports, for a candidate partial index's stored predicate text,
// whether the query predicate implies it (Q ⇒ P); only then may that partial
// index be used. A nil usePartial means partial indexes are never used here
// (e.g. ORDER BY-only scans, where a partial index could omit rows).
func findIndexOn(defs []indexDef, col string, usePartial func(partial string) bool) (indexDef, bool) {
	var leading indexDef
	found := false
	for _, d := range defs {
		// Expression indexes are not used for plain column lookups.
		if d.isExpr() {
			continue
		}
		// Partial index: only usable when the query predicate implies its predicate.
		if d.Where != "" {
			if usePartial == nil || !usePartial(d.Where) {
				continue
			}
		}
		if len(d.Columns) == 1 && strings.EqualFold(d.Columns[0], col) {
			return d, true // exact single-column index — best
		}
		if !found && len(d.Columns) > 0 && strings.EqualFold(d.Columns[0], col) {
			leading, found = d, true
		}
	}
	return leading, found
}

// findIndexForEquality returns an index all of whose columns have an equality
// constraint in eqCols (a full composite-key lookup), preferring more columns.
// usePartial governs partial-index usability as in findIndexOn.
func findIndexForEquality(defs []indexDef, eqCols map[string]bool, usePartial func(partial string) bool) (indexDef, bool) {
	best := indexDef{}
	found := false
	for _, d := range defs {
		if d.isExpr() {
			continue // expression indexes are matched via findExprIndexForEquality
		}
		if d.Where != "" {
			if usePartial == nil || !usePartial(d.Where) {
				continue
			}
		}
		all := len(d.Columns) > 0
		for _, c := range d.Columns {
			if !eqCols[strings.ToLower(c)] {
				all = false
				break
			}
		}
		if all && len(d.Columns) > len(best.Columns) {
			best, found = d, true
		}
	}
	return best, found
}

// execCreateIndex persists an index definition and back-fills it by scanning
// the existing rows and writing one IndexKey entry per row.
func (e *execImpl) execCreateIndex(ctx context.Context, s *ast.IndexStmt, sess *session.Session) (*Result, error) {
	// Build the index definition from Elems (per-element column or expression),
	// falling back to the legacy Columns list. Columns and Exprs are kept parallel
	// so the key encoder can mix column and expression elements positionally.
	var columns []string
	var exprs []string
	hasExpr := false
	if len(s.Elems) > 0 {
		for _, el := range s.Elems {
			if el.Expr != nil {
				txt, derr := deparseExpr(el.Expr)
				if derr != nil {
					return nil, derr
				}
				columns = append(columns, "")
				exprs = append(exprs, txt)
				hasExpr = true
			} else {
				columns = append(columns, el.ColName)
				exprs = append(exprs, "")
			}
		}
	} else {
		columns = s.Columns
		exprs = make([]string, len(columns))
	}
	if len(columns) == 0 {
		return nil, newExecError("42601", "index must name at least one column or expression")
	}

	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	sch, err := e.loadSchema(ctx, txn, sess, s.Table)
	if err != nil {
		return nil, err
	}
	// Validate plain-column elements exist (expression elements are validated by
	// evaluation during back-fill).
	for i, col := range columns {
		if exprs[i] != "" || col == "" {
			continue
		}
		if sch.colIndex(col) < 0 {
			return nil, newExecError("42703", "column %q does not exist in %q", col, s.Table)
		}
	}

	// Deparse the partial-index predicate, if any.
	whereTxt := ""
	if s.Where != nil {
		whereTxt, err = deparseExpr(s.Where)
		if err != nil {
			return nil, err
		}
	}

	defs, err := e.loadIndexes(ctx, txn, sess, s.Table)
	if err != nil {
		return nil, err
	}
	for _, d := range defs {
		if strings.EqualFold(d.Name, s.Name) {
			return nil, newExecError("42P07", "index %q already exists", s.Name)
		}
	}
	nd := indexDef{Name: s.Name, Table: s.Table, Columns: columns, Unique: s.Unique, Where: whereTxt}
	if hasExpr {
		nd.Exprs = exprs
	}
	defs = append(defs, nd)

	raw, err := json.Marshal(defs)
	if err != nil {
		return nil, newExecError("XX000", "encode index metadata: %v", err)
	}
	e.txn.Buffer(txn, transactions.Mutation{Key: e.indexListKey(sess, s.Table), Value: raw})

	// Back-fill: scan current rows and write one tuple-keyed entry per row,
	// honoring the partial predicate and expression elements.
	sc, err := e.scanTable(ctx, txn, sess, s.Table, s.Table)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, r := range sc.rows {
		if nd.Where != "" {
			in, perr := e.rowMatchesPartial(sch, nd, r.cells)
			if perr != nil {
				return nil, perr
			}
			if !in {
				continue
			}
		}
		encTuple, ok, err := e.encodeIndexKey(sch, nd, r.cells)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // a NULL column/expression: not indexed
		}
		if s.Unique {
			if seen[string(encTuple)] {
				return nil, newExecError("23505", "could not create unique index %q: duplicate key", s.Name)
			}
			seen[string(encTuple)] = true
		}
		pk := primaryKeyBytes(sch, r.cells, r.key)
		ik := e.store.Encoder().IndexKey(sess.Namespace(), sess.Branch(), s.Table, s.Name, encTuple, pk)
		e.txn.Buffer(txn, transactions.Mutation{Key: ik, Value: append([]byte(nil), pk...)})
	}

	if err := e.commitTx(ctx, txn, owns); err != nil {
		return nil, err
	}
	return &Result{Command: "CREATE INDEX"}, nil
}

// primaryKeyBytes returns the row's primary-key bytes (for index → pk mapping).
// Falls back to the row's full key tail when the table has no declared PK.
func primaryKeyBytes(sch *tableSchema, cells []Datum, rowKey storage.Key) []byte {
	if sch.PKIndex >= 0 && !cells[sch.PKIndex].Null {
		return []byte(cells[sch.PKIndex].Text)
	}
	return rowKey.Bytes()
}

// indexEntries writes (add=true) or deletes (add=false) the index entries for a
// row across all of a table's indexes, within an open transaction. Values are
// memcomparable-encoded per the indexed column's type (ADR-022).
func (e *execImpl) indexEntries(txn *transactions.Txn, sess *session.Session, sch *tableSchema, defs []indexDef, cells []Datum, rowKey storage.Key, add bool) error {
	pk := primaryKeyBytes(sch, cells, rowKey)
	enc := e.store.Encoder()
	for _, d := range defs {
		// Partial index: only rows satisfying the predicate are indexed.
		if d.Where != "" {
			in, err := e.rowMatchesPartial(sch, d, cells)
			if err != nil {
				return err
			}
			if !in {
				continue
			}
		}
		encTuple, ok, err := e.encodeIndexKey(sch, d, cells)
		if err != nil {
			return err
		}
		if !ok {
			continue // a NULL indexed column/expression: no entry
		}
		ik := enc.IndexKey(sess.Namespace(), sess.Branch(), d.Table, d.Name, encTuple, pk)
		if add {
			e.txn.Buffer(txn, transactions.Mutation{Key: ik, Value: append([]byte(nil), pk...)})
		} else {
			e.txn.Buffer(txn, transactions.Mutation{Key: ik, Delete: true})
		}
	}
	return nil
}

// encodeIndexKey produces the memcomparable key bytes for an index over a row,
// handling both plain-column and expression elements. ok=false when any element
// is NULL (NULLs are not indexed). Expression values are encoded as text.
func (e *execImpl) encodeIndexKey(sch *tableSchema, d indexDef, cells []Datum) (enc []byte, ok bool, err error) {
	if !d.isExpr() {
		return encodeIndexTuple(sch, d.Columns, cells)
	}
	for i := range d.Columns {
		var part []byte
		if i < len(d.Exprs) && d.Exprs[i] != "" {
			node, perr := parseStoredExpr(d.Exprs[i])
			if perr != nil {
				return nil, false, perr
			}
			ev := &evaluator{resolveCol: rowResolver(sch, d.Table, cells)}
			v, eerr := ev.eval(node)
			if eerr != nil {
				return nil, false, eerr
			}
			if v.null {
				return nil, false, nil
			}
			oid := v.oid
			if oid == 0 {
				oid = OIDText
			}
			p, encErr := orderEncode(oid, v.text)
			if encErr != nil {
				return nil, false, encErr
			}
			part = p
		} else {
			col := d.Columns[i]
			ci := sch.colIndex(col)
			if ci < 0 || cells[ci].Null {
				return nil, false, nil
			}
			p, encErr := orderEncode(sch.Cols[ci].TypeOID, cells[ci].Text)
			if encErr != nil {
				return nil, false, encErr
			}
			part = p
		}
		enc = append(enc, part...)
	}
	return enc, true, nil
}

// exprMatchesIndex reports whether a query expression structurally matches the
// (single) expression element of an expression index. Both sides are normalized
// by deparsing to canonical SQL text and compared; this absorbs whitespace and
// parenthesization differences. Only single-expression indexes are matched for
// lookups (the common case: a functional index on one expression).
func exprMatchesIndex(d indexDef, queryExpr ast.Node) bool {
	if !d.isExpr() {
		return false
	}
	// Find the lone expression element; require the index to have exactly one
	// (expression) element so a single `<expr> = const` can drive it.
	idxExpr := ""
	count := 0
	for _, e := range d.Exprs {
		if e != "" {
			idxExpr = e
			count++
		}
	}
	if count != 1 || len(d.Columns) != 1 {
		return false
	}
	qTxt, err := deparseExpr(queryExpr)
	if err != nil {
		return false
	}
	// idxExpr is already deparsed (canonical) text; re-parse + re-deparse the
	// stored text so both sides pass through the identical canonicalizer.
	idxNode, perr := parseStoredExpr(idxExpr)
	if perr != nil {
		return false
	}
	idxCanon, derr := deparseExpr(idxNode)
	if derr != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(qTxt), strings.TrimSpace(idxCanon))
}

// findExprIndexForEquality returns an expression index whose stored expression
// structurally matches queryExpr (used to serve `<indexed-expr> = const`). Only
// non-partial expression indexes are considered here; partial expression indexes
// are not used for lookups (conservative — no predicate-implication check).
func findExprIndexForEquality(defs []indexDef, queryExpr ast.Node) (indexDef, bool) {
	for _, d := range defs {
		if d.Where != "" {
			continue
		}
		if exprMatchesIndex(d, queryExpr) {
			return d, true
		}
	}
	return indexDef{}, false
}

// predicateImplies reports whether query predicate Q implies partial-index
// predicate P (Q ⇒ P), i.e. every row satisfying Q also satisfies P, so the
// partial index — which contains exactly the rows satisfying P — is safe to use.
//
// This is deliberately conservative: it returns true only for cases it can prove
// by structural matching, and false otherwise (forcing a seq scan). Recognized
// cases:
//   - Q contains P as a top-level conjunct (Q = ... AND P AND ...), comparing the
//     deparsed canonical forms of each conjunct against P.
//   - P is a bare boolean column `c` (or `c = true`) and Q contains the conjunct
//     `c`, `c = true`, or `c IS TRUE` (and the symmetric true=c form).
func predicateImplies(queryPred ast.Node, partialText string) bool {
	if partialText == "" {
		return true // a full index has no predicate to imply
	}
	if queryPred == nil {
		return false
	}
	pNode, err := parseStoredExpr(partialText)
	if err != nil {
		return false
	}
	pCanon, derr := deparseExpr(pNode)
	if derr != nil {
		return false
	}
	pCanon = strings.TrimSpace(pCanon)
	// Canonical alternative forms that all mean the same as a bare boolean P.
	pAlts := booleanEquivForms(pNode, pCanon)

	matches := func(conjunct ast.Node) bool {
		ctxt, e := deparseExpr(conjunct)
		if e != nil {
			return false
		}
		ctxt = strings.TrimSpace(ctxt)
		for _, alt := range pAlts {
			if strings.EqualFold(ctxt, alt) {
				return true
			}
		}
		// Also expand the conjunct's own boolean-equivalent forms so e.g. a query
		// `active = true` matches a partial predicate `active`.
		for _, calt := range booleanEquivForms(conjunct, ctxt) {
			for _, alt := range pAlts {
				if strings.EqualFold(calt, alt) {
					return true
				}
			}
		}
		return false
	}

	// Walk Q's top-level AND conjuncts (a non-AND Q is treated as a single
	// conjunct). OR is not descended into — an OR branch need not satisfy P.
	var found bool
	var walk func(n ast.Node)
	walk = func(n ast.Node) {
		if found {
			return
		}
		if be, ok := n.(*ast.BoolExpr); ok && be.Op == ast.AND_EXPR {
			for _, a := range be.Args {
				walk(a)
			}
			return
		}
		if matches(n) {
			found = true
		}
	}
	walk(queryPred)
	return found
}

// booleanEquivForms returns the canonical deparsed strings that are all
// equivalent to a boolean predicate node, so a partial predicate `active` and a
// query conjunct `active = true` (or `active IS TRUE`) match. canon is the node's
// own deparsed form (always included).
func booleanEquivForms(n ast.Node, canon string) []string {
	forms := []string{canon}
	// Bare column reference c  ⇔  (c = TRUE)  ⇔  (c IS TRUE).
	if cr, ok := n.(*ast.ColumnRef); ok {
		col := strings.Join(cr.Fields, ".")
		forms = append(forms, "("+col+" = TRUE)", "("+col+" IS TRUE)")
		return forms
	}
	// (c = TRUE) / (TRUE = c)  ⇔  c.
	if ae, ok := n.(*ast.A_Expr); ok && ae.Kind == ast.AEXPR_OP && ae.Name == "=" {
		var colNode, constNode ast.Node = ae.Lexpr, ae.Rexpr
		if isTrueConst(ae.Lexpr) {
			colNode, constNode = ae.Rexpr, ae.Lexpr
		}
		if cr, ok := colNode.(*ast.ColumnRef); ok && isTrueConst(constNode) {
			col := strings.Join(cr.Fields, ".")
			forms = append(forms, col, "("+col+" = TRUE)", "("+col+" IS TRUE)")
		}
	}
	return forms
}

// isTrueConst reports whether n is the boolean constant TRUE.
func isTrueConst(n ast.Node) bool {
	c, ok := n.(*ast.A_Const)
	return ok && c.Type == ast.ConstBool && c.Val == "true"
}

// rowMatchesPartial evaluates a partial index's WHERE predicate against a row.
func (e *execImpl) rowMatchesPartial(sch *tableSchema, d indexDef, cells []Datum) (bool, error) {
	node, err := parseStoredExpr(d.Where)
	if err != nil {
		return false, err
	}
	ev := &evaluator{resolveCol: rowResolver(sch, d.Table, cells)}
	v, err := ev.eval(node)
	if err != nil {
		return false, err
	}
	return asBool(v), nil
}
