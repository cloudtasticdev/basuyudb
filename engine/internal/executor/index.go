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

// indexDef is a persisted secondary-index definition. V0.2 supports
// single-column indexes (the common point-lookup case); multi-column indexes
// are a later extension.
type indexDef struct {
	Name   string `json:"name"`
	Table  string `json:"table"`
	Column string `json:"column"`
	Unique bool   `json:"unique"`
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

// findIndexOn returns the index covering exactly column col, if any.
func findIndexOn(defs []indexDef, col string) (indexDef, bool) {
	for _, d := range defs {
		if strings.EqualFold(d.Column, col) {
			return d, true
		}
	}
	return indexDef{}, false
}

// execCreateIndex persists an index definition and back-fills it by scanning
// the existing rows and writing one IndexKey entry per row.
func (e *execImpl) execCreateIndex(ctx context.Context, s *ast.IndexStmt, sess *session.Session) (*Result, error) {
	if len(s.Columns) != 1 {
		return nil, newExecError("0A000", "only single-column indexes are supported in V0.2")
	}
	col := s.Columns[0]

	txn, err := e.txn.Begin(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.txn.Rollback(ctx, txn)

	sch, err := e.loadSchema(ctx, txn, sess, s.Table)
	if err != nil {
		return nil, err
	}
	if sch.colIndex(col) < 0 {
		return nil, newExecError("42703", "column %q does not exist in %q", col, s.Table)
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
	defs = append(defs, indexDef{Name: s.Name, Table: s.Table, Column: col, Unique: s.Unique})

	raw, err := json.Marshal(defs)
	if err != nil {
		return nil, newExecError("XX000", "encode index metadata: %v", err)
	}
	e.txn.Buffer(txn, transactions.Mutation{Key: e.indexListKey(sess, s.Table), Value: raw})

	// Back-fill: scan current rows and write index entries.
	sc, err := e.scanTable(ctx, txn, sess, s.Table, s.Table)
	if err != nil {
		return nil, err
	}
	colIdx := sch.colIndex(col)
	seen := map[string]bool{}
	for _, r := range sc.rows {
		val := r.cells[colIdx]
		if s.Unique && !val.Null {
			if seen[val.Text] {
				return nil, newExecError("23505", "could not create unique index %q: duplicate value %q", s.Name, val.Text)
			}
			seen[val.Text] = true
		}
		pk := primaryKeyBytes(sch, r.cells, r.key)
		ik := e.store.Encoder().IndexKey(sess.Namespace(), sess.Branch(), s.Table, col, []byte(val.Text), pk)
		// The entry value carries the row's pk so an index scan fetches the row
		// with a single point-get (no key parsing).
		e.txn.Buffer(txn, transactions.Mutation{Key: ik, Value: append([]byte(nil), pk...)})
	}

	if err := e.txn.Commit(ctx, txn); err != nil {
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
// row across all of a table's indexes, within an open transaction.
func (e *execImpl) indexEntries(txn *transactions.Txn, sess *session.Session, sch *tableSchema, defs []indexDef, cells []Datum, rowKey storage.Key, add bool) {
	pk := primaryKeyBytes(sch, cells, rowKey)
	enc := e.store.Encoder()
	for _, d := range defs {
		ci := sch.colIndex(d.Column)
		if ci < 0 || cells[ci].Null {
			continue
		}
		ik := enc.IndexKey(sess.Namespace(), sess.Branch(), d.Table, d.Column, []byte(cells[ci].Text), pk)
		if add {
			e.txn.Buffer(txn, transactions.Mutation{Key: ik, Value: append([]byte(nil), pk...)})
		} else {
			e.txn.Buffer(txn, transactions.Mutation{Key: ik, Delete: true})
		}
	}
}
