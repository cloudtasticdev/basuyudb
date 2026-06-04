package executor

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// isRelMissing reports whether err is a "relation does not exist" (42P01).
func isRelMissing(err error) bool {
	if ee, ok := err.(*ExecError); ok {
		return ee.SQLSTATE == "42P01"
	}
	return false
}

// deletePrefix buffers a delete for every committed key under prefix on the
// session's branch (the iterator reads the snapshot, deletes go to the buffer).
func (e *execImpl) deletePrefix(txn *transactions.Txn, prefix storage.Key) {
	it := e.txn.NewIterator(txn, prefix)
	defer it.Close()
	for it.Rewind(); it.Valid(); it.Next() {
		e.txn.Buffer(txn, transactions.Mutation{Key: it.Key(), Delete: true})
	}
}

// dropTableData removes all rows and index entries of a table on the session's
// branch (shared by DROP TABLE and TRUNCATE).
func (e *execImpl) dropTableData(ctx context.Context, txn *transactions.Txn, sess *session.Session, table string) error {
	enc := e.store.Encoder()
	e.deletePrefix(txn, enc.RowPrefix(sess.Namespace(), sess.Branch(), table))
	defs, err := e.loadIndexes(ctx, txn, sess, table)
	if err != nil {
		return err
	}
	for _, d := range defs {
		e.deletePrefix(txn, enc.IndexColumnPrefix(sess.Namespace(), sess.Branch(), d.Table, d.Name))
	}
	return nil
}

// execDropTable removes a table's schema, index definitions, rows, and index
// entries. DROP TABLE IF EXISTS is a no-op when the table is absent.
func (e *execImpl) execDropTable(ctx context.Context, s *ast.DropStmt, sess *session.Session) (*Result, error) {
	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	if _, err := e.loadSchema(ctx, txn, sess, s.Table); err != nil {
		if s.IfExists && isRelMissing(err) {
			return &Result{Command: "DROP TABLE"}, nil
		}
		return nil, err
	}
	if err := e.dropTableData(ctx, txn, sess, s.Table); err != nil {
		return nil, err
	}
	enc := e.store.Encoder()
	e.txn.Buffer(txn, transactions.Mutation{Key: e.indexListKey(sess, s.Table), Delete: true})
	e.txn.Buffer(txn, transactions.Mutation{Key: enc.SchemaKey(sess.Namespace(), s.Table), Delete: true})
	if err := e.commitTx(ctx, txn, owns); err != nil {
		return nil, err
	}
	return &Result{Command: "DROP TABLE"}, nil
}

// execTruncate removes all rows and index entries but keeps the schema and
// index definitions.
func (e *execImpl) execTruncate(ctx context.Context, s *ast.TruncateStmt, sess *session.Session) (*Result, error) {
	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	if _, err := e.loadSchema(ctx, txn, sess, s.Table); err != nil {
		return nil, err
	}
	if err := e.dropTableData(ctx, txn, sess, s.Table); err != nil {
		return nil, err
	}
	if err := e.commitTx(ctx, txn, owns); err != nil {
		return nil, err
	}
	return &Result{Command: "TRUNCATE TABLE"}, nil
}

// execAlterTable applies ADD COLUMN / DROP COLUMN to a table's schema. ADD is
// online (existing rows read the new column as NULL via decodeRow padding).
// DROP rewrites every row without the column and removes any index on it.
func (e *execImpl) execAlterTable(ctx context.Context, s *ast.AlterTableStmt, sess *session.Session) (*Result, error) {
	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	sch, err := e.loadSchema(ctx, txn, sess, s.Table)
	if err != nil {
		return nil, err
	}

	switch s.Kind {
	case ast.AlterAddColumn:
		if sch.colIndex(s.Column.ColName) >= 0 {
			return nil, newExecError("42701", "column %q of relation %q already exists", s.Column.ColName, s.Table)
		}
		if s.Column.PrimaryKey {
			return nil, newExecError("0A000", "ADD COLUMN ... PRIMARY KEY is not supported")
		}
		sch.Cols = append(sch.Cols, colMeta{
			Name:    s.Column.ColName,
			TypeOID: oidForTypeName(s.Column.TypeName),
			NotNull: s.Column.NotNull,
		})

	case ast.AlterDropColumn:
		idx := sch.colIndex(s.Column.ColName)
		if idx < 0 {
			return nil, newExecError("42703", "column %q of relation %q does not exist", s.Column.ColName, s.Table)
		}
		if sch.PKIndex == idx {
			return nil, newExecError("0A000", "cannot drop primary key column %q", s.Column.ColName)
		}
		// Rewrite each row without the dropped cell.
		sc, err := e.scanTable(ctx, txn, sess, s.Table, s.Table)
		if err != nil {
			return nil, err
		}
		for _, r := range sc.rows {
			newCells := make([]Datum, 0, len(r.cells)-1)
			for i, c := range r.cells {
				if i != idx {
					newCells = append(newCells, c)
				}
			}
			e.txn.Buffer(txn, transactions.Mutation{Key: r.key, Value: encodeRow(newCells)})
		}
		// Drop any index on the column and its entries.
		defs, err := e.loadIndexes(ctx, txn, sess, s.Table)
		if err != nil {
			return nil, err
		}
		kept := defs[:0]
		for _, d := range defs {
			if d.hasColumn(s.Column.ColName) {
				// Any index covering the dropped column is dropped entirely.
				e.deletePrefix(txn, e.store.Encoder().IndexColumnPrefix(sess.Namespace(), sess.Branch(), d.Table, d.Name))
				continue
			}
			kept = append(kept, d)
		}
		if len(kept) != len(defs) {
			raw, _ := json.Marshal(kept)
			e.txn.Buffer(txn, transactions.Mutation{Key: e.indexListKey(sess, s.Table), Value: raw})
		}
		// Remove the column; shift PKIndex if it followed the dropped column.
		sch.Cols = append(sch.Cols[:idx], sch.Cols[idx+1:]...)
		if sch.PKIndex > idx {
			sch.PKIndex--
		}

	case ast.AlterEnableRLS:
		sch.RLSEnabled = true
	case ast.AlterDisableRLS:
		sch.RLSEnabled = false
	case ast.AlterForceRLS:
		sch.RLSForced = true
	case ast.AlterNoForceRLS:
		sch.RLSForced = false

	case ast.AlterRenameTable:
		// RENAME TO: tables are keyed by name (no stable OID), so migrate the
		// schema, index metadata, every row, and every index entry to the new name
		// prefix, then remove the old keys. Data is accessible under the new name
		// and gone from the old.
		if err := e.renameTable(ctx, txn, sess, s.Table, s.NewName, sch); err != nil {
			return nil, err
		}
		if err := e.commitTx(ctx, txn, owns); err != nil {
			return nil, err
		}
		return &Result{Command: "ALTER TABLE"}, nil
	}

	// Process multi-action Cmds (RENAME COLUMN, ALTER COLUMN ..., ADD CONSTRAINT).
	for _, cmd := range s.Cmds {
		switch cmd.Subtype {
		case ast.AlterRenameColumn:
			oldName := strings.ToLower(cmd.Name)
			newName := strings.ToLower(cmd.NewName)
			found := false
			for i, col := range sch.Cols {
				if strings.EqualFold(col.Name, oldName) {
					sch.Cols[i].Name = newName
					found = true
					break
				}
			}
			if !found {
				return nil, newExecError("42703", "column %q of relation %q does not exist", oldName, s.Table)
			}

		case ast.AlterColumnType:
			// Update the declared type/OID AND physically rewrite every stored row,
			// casting column cmd.Name's value to the new type (or evaluating the
			// USING expr per row when present). The rewrite is staged into the txn
			// and committed atomically with the schema change, so a cast failure
			// aborts the whole ALTER without partial corruption.
			ci := sch.colIndex(cmd.Name)
			if ci < 0 {
				return nil, newExecError("42703", "column %q of relation %q does not exist", cmd.Name, s.Table)
			}
			newOID := oidForTypeName(cmd.TypeName)
			if err := e.rewriteColumnType(ctx, txn, sess, sch, s.Table, ci, newOID, cmd.UsingExpr); err != nil {
				return nil, err
			}
			sch.Cols[ci].TypeOID = newOID

		case ast.AlterSetDefault:
			ci := sch.colIndex(cmd.Name)
			if ci < 0 {
				return nil, newExecError("42703", "column %q of relation %q does not exist", cmd.Name, s.Table)
			}
			ds, err := classifyDefault(cmd.DefExpr)
			if err != nil {
				return nil, err
			}
			sch.Cols[ci].Default = ds

		case ast.AlterDropDefault:
			ci := sch.colIndex(cmd.Name)
			if ci < 0 {
				return nil, newExecError("42703", "column %q of relation %q does not exist", cmd.Name, s.Table)
			}
			sch.Cols[ci].Default = nil

		case ast.AlterSetNotNull:
			ci := sch.colIndex(cmd.Name)
			if ci < 0 {
				return nil, newExecError("42703", "column %q of relation %q does not exist", cmd.Name, s.Table)
			}
			sch.Cols[ci].NotNull = true

		case ast.AlterDropNotNull:
			ci := sch.colIndex(cmd.Name)
			if ci < 0 {
				return nil, newExecError("42703", "column %q of relation %q does not exist", cmd.Name, s.Table)
			}
			sch.Cols[ci].NotNull = false

		case ast.AlterAddConstraint:
			if err := e.alterAddConstraint(ctx, txn, sess, sch, s.Table, cmd.Constraint); err != nil {
				return nil, err
			}

		case ast.AlterEnableRLS:
			sch.RLSEnabled = true
		case ast.AlterDisableRLS:
			sch.RLSEnabled = false
		case ast.AlterForceRLS:
			sch.RLSForced = true
		case ast.AlterNoForceRLS:
			sch.RLSForced = false
		}
	}

	raw, err := json.Marshal(sch)
	if err != nil {
		return nil, newExecError("XX000", "encode schema: %v", err)
	}
	e.txn.Buffer(txn, transactions.Mutation{Key: e.store.Encoder().SchemaKey(sess.Namespace(), s.Table), Value: raw})
	if err := e.commitTx(ctx, txn, owns); err != nil {
		return nil, err
	}
	return &Result{Command: "ALTER TABLE"}, nil
}

// rewriteColumnType physically converts every stored row's value for column ci
// to the new type newOID (or to the value of usingExpr evaluated per row when
// usingExpr is non-nil), staging the rewritten rows and refreshed index entries
// into txn. A value that cannot be cast aborts with a clear error (22P02 / 42846)
// and leaves the txn unconverted (the caller rolls back), so no partial
// corruption occurs.
func (e *execImpl) rewriteColumnType(ctx context.Context, txn *transactions.Txn, sess *session.Session, sch *tableSchema, table string, ci int, newOID uint32, usingExpr ast.Node) error {
	sc, err := e.scanTable(ctx, txn, sess, table, table)
	if err != nil {
		return err
	}
	defs, err := e.loadIndexes(ctx, txn, sess, table)
	if err != nil {
		return err
	}
	// Only indexes covering the altered column need their entries refreshed.
	var affected []indexDef
	for _, d := range defs {
		if d.hasColumn(sch.Cols[ci].Name) || d.isExpr() {
			affected = append(affected, d)
		}
	}

	// First pass: compute every converted row (failing fast on a bad cast before
	// any mutation is staged), and delete affected index entries keyed under the
	// OLD column type.
	converted := make([][]Datum, len(sc.rows))
	for i, r := range sc.rows {
		old := r.cells[ci]
		var nv Datum
		if usingExpr != nil {
			ev := &evaluator{
				resolveCol:      rowResolver(sch, table, r.cells),
				lookupComposite: e.compositeResolverCtx(ctx, sess),
			}
			v, eerr := ev.eval(usingExpr)
			if eerr != nil {
				return eerr
			}
			if v.null {
				nv = Datum{Null: true}
			} else {
				txt, cerr := castTextToOID(v.text, newOID)
				if cerr != nil {
					return cerr
				}
				nv = Datum{Text: txt}
			}
		} else if old.Null {
			nv = Datum{Null: true}
		} else {
			txt, cerr := castTextToOID(old.Text, newOID)
			if cerr != nil {
				return cerr
			}
			nv = Datum{Text: txt}
		}
		newCells := append([]Datum(nil), r.cells...)
		newCells[ci] = nv
		converted[i] = newCells

		// Remove old index entries while the schema still carries the OLD OID.
		if len(affected) > 0 {
			if derr := e.indexEntries(txn, sess, sch, affected, r.cells, r.key, false); derr != nil {
				return derr
			}
		}
	}

	// Flip the schema's column OID so the index encoder keys new entries under the
	// NEW type, then write rows and add refreshed entries.
	sch.Cols[ci].TypeOID = newOID
	for i, r := range sc.rows {
		e.txn.Buffer(txn, transactions.Mutation{Key: r.key, Value: encodeRow(converted[i])})
		if len(affected) > 0 {
			if derr := e.indexEntries(txn, sess, sch, affected, converted[i], r.key, true); derr != nil {
				return derr
			}
		}
	}
	return nil
}

// castTextToOID validates and normalizes a PG text value to the canonical text
// form of the target type OID, as ALTER COLUMN TYPE / CAST would. It returns a
// clear error (22P02 invalid input syntax, or 42846 cannot cast) when the value
// is not representable in the target type. Text-family targets accept anything.
func castTextToOID(text string, oid uint32) (string, error) {
	s := strings.TrimSpace(text)
	switch oid {
	case OIDInt2:
		n, err := strconv.ParseInt(s, 10, 16)
		if err != nil {
			return "", newExecError("22P02", "invalid input syntax for type smallint: %q", text)
		}
		return strconv.FormatInt(n, 10), nil
	case OIDInt4:
		n, err := strconv.ParseInt(s, 10, 32)
		if err != nil {
			return "", newExecError("22P02", "invalid input syntax for type integer: %q", text)
		}
		return strconv.FormatInt(n, 10), nil
	case OIDInt8, OIDOid:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return "", newExecError("22P02", "invalid input syntax for type bigint: %q", text)
		}
		return strconv.FormatInt(n, 10), nil
	case OIDFloat4:
		f, err := strconv.ParseFloat(s, 32)
		if err != nil {
			return "", newExecError("22P02", "invalid input syntax for type real: %q", text)
		}
		return strconv.FormatFloat(f, 'g', -1, 32), nil
	case OIDFloat8:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return "", newExecError("22P02", "invalid input syntax for type double precision: %q", text)
		}
		return strconv.FormatFloat(f, 'g', -1, 64), nil
	case OIDNumeric:
		// Accept any well-formed decimal; keep the source text (canonical enough).
		if _, ok := encodeCopyNumeric(s); !ok && !strings.EqualFold(s, "nan") {
			return "", newExecError("22P02", "invalid input syntax for type numeric: %q", text)
		}
		return s, nil
	case OIDBool:
		switch strings.ToLower(s) {
		case "t", "true", "1", "yes", "y", "on":
			return "t", nil
		case "f", "false", "0", "no", "n", "off":
			return "f", nil
		}
		return "", newExecError("22P02", "invalid input syntax for type boolean: %q", text)
	case OIDDate:
		if _, ok := copyParseTime(s); !ok {
			return "", newExecError("22P02", "invalid input syntax for type date: %q", text)
		}
		return s, nil
	case OIDTimestamp, OIDTimestamptz:
		if _, ok := copyParseTime(s); !ok {
			return "", newExecError("22P02", "invalid input syntax for type timestamp: %q", text)
		}
		return s, nil
	case OIDUUID:
		if _, ok := encodeCopyUUID(s); !ok {
			return "", newExecError("22P02", "invalid input syntax for type uuid: %q", text)
		}
		return strings.ToLower(s), nil
	default:
		// text/varchar/bpchar/name/json/jsonb/bytea and unknown: accept the value
		// as-is (any value has a valid text representation in these types).
		return text, nil
	}
}
