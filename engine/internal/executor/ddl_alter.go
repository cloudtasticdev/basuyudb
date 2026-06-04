package executor

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// renameTable migrates a table's schema, index metadata, rows, and index entries
// from oldName to newName. Tables are keyed by name throughout the key space
// (there is no stable table OID), so a rename is a physical copy + delete of
// every keyed artifact on the session's branch. The caller persists the (renamed)
// schema; renameTable writes the new-name schema itself and removes the old one.
func (e *execImpl) renameTable(ctx context.Context, txn *transactions.Txn, sess *session.Session, oldName, newName string, sch *tableSchema) error {
	if strings.EqualFold(oldName, newName) {
		return nil
	}
	if _, err := e.loadSchema(ctx, txn, sess, newName); err == nil {
		return newExecError("42P07", "relation %q already exists", newName)
	}
	enc := e.store.Encoder()

	// Load index definitions before we start mutating.
	defs, err := e.loadIndexes(ctx, txn, sess, oldName)
	if err != nil {
		return err
	}

	// Materialize the full logical row set (merges branch fall-through) and
	// re-key each row under the new table name on the session's branch.
	sc, err := e.scanTable(ctx, txn, sess, oldName, oldName)
	if err != nil {
		return err
	}
	for _, r := range sc.rows {
		pk := primaryKeyBytes(sch, r.cells, r.key)
		newKey := enc.RowKey(sess.Namespace(), sess.Branch(), newName, pk)
		e.txn.Buffer(txn, transactions.Mutation{Key: newKey, Value: encodeRow(r.cells)})
	}

	// Rebuild index entries under the new table name; drop the old index column
	// prefixes. Index defs carry the (old) table name — update it to newName.
	newDefs := make([]indexDef, len(defs))
	for i, d := range defs {
		newDefs[i] = d
		newDefs[i].Table = newName
		// Drop old entries.
		e.deletePrefix(txn, enc.IndexColumnPrefix(sess.Namespace(), sess.Branch(), d.Table, d.Name))
	}
	for _, r := range sc.rows {
		if err := e.indexEntries(txn, sess, sch, newDefs, r.cells, r.key, true); err != nil {
			return err
		}
	}
	if len(newDefs) > 0 {
		raw, err := json.Marshal(newDefs)
		if err != nil {
			return newExecError("XX000", "encode index metadata: %v", err)
		}
		e.txn.Buffer(txn, transactions.Mutation{Key: e.indexListKey(sess, newName), Value: raw})
	}
	// Remove old index metadata.
	e.txn.Buffer(txn, transactions.Mutation{Key: e.indexListKey(sess, oldName), Delete: true})

	// Delete old rows. On a feature branch, parent rows are tombstoned so the
	// table no longer resolves under the old name; on main they are removed.
	onBranch := sess.Branch() != "main"
	for _, r := range sc.rows {
		pk := primaryKeyBytes(sch, r.cells, r.key)
		oldKey := enc.RowKey(sess.Namespace(), sess.Branch(), oldName, pk)
		if onBranch {
			e.txn.Buffer(txn, transactions.Mutation{Key: oldKey, Value: storage.Tombstone()})
		} else {
			e.txn.Buffer(txn, transactions.Mutation{Key: oldKey, Delete: true})
		}
	}

	// Write the renamed schema and delete the old schema entry.
	sch.Name = newName
	raw, err := json.Marshal(sch)
	if err != nil {
		return newExecError("XX000", "encode schema: %v", err)
	}
	e.txn.Buffer(txn, transactions.Mutation{Key: enc.SchemaKey(sess.Namespace(), newName), Value: raw})
	e.txn.Buffer(txn, transactions.Mutation{Key: enc.SchemaKey(sess.Namespace(), oldName), Delete: true})
	return nil
}

// alterAddConstraint applies ALTER TABLE ... ADD [CONSTRAINT name] { PRIMARY KEY
// | UNIQUE | FOREIGN KEY | CHECK }. PRIMARY KEY/UNIQUE build a backing unique
// index; FOREIGN KEY records the FK (with referential actions) on the local
// column(s); CHECK stores the deparsed predicate so it is enforced on write. The
// caller persists sch afterwards, so catalog edits made here are saved.
func (e *execImpl) alterAddConstraint(ctx context.Context, txn *transactions.Txn, sess *session.Session, sch *tableSchema, table string, c *ast.AlterTableConstraint) error {
	if c == nil {
		return newExecError("XX000", "ADD CONSTRAINT missing detail")
	}
	switch c.ConstraintType {
	case ast.ConstrPrimaryKey, ast.ConstrUnique:
		for _, col := range c.Columns {
			if sch.colIndex(col) < 0 {
				return newExecError("42703", "column %q of relation %q does not exist", col, table)
			}
		}
		name := c.Name
		if name == "" {
			suffix := "key"
			if c.ConstraintType == ast.ConstrPrimaryKey {
				suffix = "pkey"
			}
			name = table + "_" + strings.Join(c.Columns, "_") + "_" + suffix
		}
		// PRIMARY KEY also implies NOT NULL on its columns and, when the table has
		// no declared PK, sets PKIndex to a single-column key.
		if c.ConstraintType == ast.ConstrPrimaryKey {
			for _, col := range c.Columns {
				ci := sch.colIndex(col)
				sch.Cols[ci].NotNull = true
				sch.Cols[ci].PK = true
			}
			if sch.PKIndex < 0 && len(c.Columns) == 1 {
				sch.PKIndex = sch.colIndex(c.Columns[0])
			}
		}
		return e.buildUniqueIndex(ctx, txn, sess, sch, table, name, c.Columns)

	case ast.ConstrForeignKey:
		// Record the FK (with referential actions) on the anchor (first local)
		// column. Composite foreign keys store every (local, referenced) pair in
		// FKCols so enforcement matches on the full tuple (item 2).
		return recordForeignKey(sch, table, c)

	case ast.ConstrCheck:
		return recordCheck(sch, c.Expr)

	default:
		return newExecError("0A000", "unsupported ADD CONSTRAINT type")
	}
}

// buildUniqueIndex registers a unique index in the table's index list and
// back-fills it from the current rows, enforcing uniqueness. Mirrors the
// CREATE INDEX back-fill path.
func (e *execImpl) buildUniqueIndex(ctx context.Context, txn *transactions.Txn, sess *session.Session, sch *tableSchema, table, name string, columns []string) error {
	defs, err := e.loadIndexes(ctx, txn, sess, table)
	if err != nil {
		return err
	}
	for _, d := range defs {
		if strings.EqualFold(d.Name, name) {
			return newExecError("42P07", "index %q already exists", name)
		}
	}
	defs = append(defs, indexDef{Name: name, Table: table, Columns: columns, Unique: true})
	raw, err := json.Marshal(defs)
	if err != nil {
		return newExecError("XX000", "encode index metadata: %v", err)
	}
	e.txn.Buffer(txn, transactions.Mutation{Key: e.indexListKey(sess, table), Value: raw})

	sc, err := e.scanTable(ctx, txn, sess, table, table)
	if err != nil {
		return err
	}
	enc := e.store.Encoder()
	seen := map[string]bool{}
	for _, r := range sc.rows {
		encTuple, ok, err := encodeIndexTuple(sch, columns, r.cells)
		if err != nil {
			return err
		}
		if !ok {
			continue // a NULL column is not indexed
		}
		if seen[string(encTuple)] {
			return newExecError("23505", "could not create unique constraint %q: duplicate key", name)
		}
		seen[string(encTuple)] = true
		pk := primaryKeyBytes(sch, r.cells, r.key)
		ik := enc.IndexKey(sess.Namespace(), sess.Branch(), table, name, encTuple, pk)
		e.txn.Buffer(txn, transactions.Mutation{Key: ik, Value: append([]byte(nil), pk...)})
	}
	return nil
}
