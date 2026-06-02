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
func findIndexOn(defs []indexDef, col string) (indexDef, bool) {
	var leading indexDef
	found := false
	for _, d := range defs {
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
func findIndexForEquality(defs []indexDef, eqCols map[string]bool) (indexDef, bool) {
	best := indexDef{}
	found := false
	for _, d := range defs {
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
	if len(s.Columns) == 0 {
		return nil, newExecError("42601", "index must name at least one column")
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
	for _, col := range s.Columns {
		if sch.colIndex(col) < 0 {
			return nil, newExecError("42703", "column %q does not exist in %q", col, s.Table)
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
	defs = append(defs, indexDef{Name: s.Name, Table: s.Table, Columns: s.Columns, Unique: s.Unique})

	raw, err := json.Marshal(defs)
	if err != nil {
		return nil, newExecError("XX000", "encode index metadata: %v", err)
	}
	e.txn.Buffer(txn, transactions.Mutation{Key: e.indexListKey(sess, s.Table), Value: raw})

	// Back-fill: scan current rows and write one tuple-keyed entry per row.
	sc, err := e.scanTable(ctx, txn, sess, s.Table, s.Table)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, r := range sc.rows {
		encTuple, ok, err := encodeIndexTuple(sch, s.Columns, r.cells)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // a NULL column: not indexed
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
		encTuple, ok, err := encodeIndexTuple(sch, d.Columns, cells)
		if err != nil {
			return err
		}
		if !ok {
			continue // a NULL indexed column: no entry
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
