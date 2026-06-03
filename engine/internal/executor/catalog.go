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

// colMeta is a persisted column definition.
type colMeta struct {
	Name string `json:"name"`
	TypeOID uint32 `json:"type_oid"`
	NotNull bool `json:"not_null"`
	PK bool `json:"pk"`
	Unique bool `json:"unique,omitempty"`
	Default *defaultSpec `json:"default,omitempty"`
	Check string `json:"check,omitempty"` // deparsed CHECK expression text
	FKTable string `json:"fk_table,omitempty"`
	FKColumn string `json:"fk_column,omitempty"`
}

// defaultSpec is a column DEFAULT, classified at CREATE TABLE time into a small
// set of evaluable forms so INSERT can materialize a value without re-parsing.
type defaultSpec struct {
	Kind string `json:"kind"` // "const" | "now" | "uuid" | "serial"
	Text string `json:"text,omitempty"` // literal text for Kind=="const"
	OID  uint32 `json:"oid,omitempty"`  // value OID for Kind=="const"
	Seq  string `json:"seq,omitempty"`  // sequence name for Kind=="serial"
}

// tableSchema is a persisted table definition stored under SchemaKey.
type tableSchema struct {
	Name string `json:"name"`
	Cols []colMeta `json:"cols"`
	PKIndex int `json:"pk_index"` // -1 if no primary key
}

// colIndex returns the position of a column by (case-insensitive) name, or -1.
func (s *tableSchema) colIndex(name string) int {
	for i, c := range s.Cols {
		if strings.EqualFold(c.Name, name) {
			return i
		}
	}
	return -1
}

// loadSchema reads a table's schema at the txn snapshot. Returns ErrNoSuchTable
// if absent.
func (e *execImpl) loadSchema(ctx context.Context, txn *transactions.Txn, sess *session.Session, table string) (*tableSchema, error) {
	key := e.store.Encoder().SchemaKey(sess.Namespace(), table)
	raw, err := e.txn.Get(ctx, txn, key)
	if errors.Is(err, storage.ErrKeyNotFound) {
		return nil, newExecError("42P01", "relation %q does not exist", table)
	}
	if err != nil {
		return nil, err
	}
	var s tableSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, newExecError("XX000", "corrupt schema for %q: %v", table, err)
	}
	return &s, nil
}

// execCreateTable persists a new table schema. Errors if the table exists.
func (e *execImpl) execCreateTable(ctx context.Context, c *ast.CreateStmt, sess *session.Session) (*Result, error) {
	table := c.Relation.RelName

	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	if _, err := e.loadSchema(ctx, txn, sess, table); err == nil {
		return nil, newExecError("42P07", "relation %q already exists", table)
	}

	sch := tableSchema{Name: table, PKIndex: -1, Cols: make([]colMeta, 0, len(c.TableElts))}
	var uniqueIdx []indexDef
	for _, cd := range c.TableElts {
		cm := colMeta{
			Name: cd.ColName,
			TypeOID: oidForTypeName(cd.TypeName),
			NotNull: cd.NotNull,
			PK: cd.PrimaryKey,
			Unique: cd.Unique,
		}
		// SERIAL/BIGSERIAL/SMALLSERIAL: integer column + auto-increment sequence.
		if base, ok := serialBaseOID(cd.TypeName); ok {
			cm.TypeOID = base
			cm.NotNull = true
			cm.Default = &defaultSpec{Kind: "serial", Seq: table + "_" + cd.ColName + "_seq"}
		} else if cd.Default != nil {
			ds, err := classifyDefault(cd.Default)
			if err != nil {
				return nil, err
			}
			cm.Default = ds
		}
		// Foreign-key and CHECK constraints.
		if cd.FKTable != "" {
			cm.FKTable = cd.FKTable
			cm.FKColumn = cd.FKColumn
		}
		if cd.Check != nil {
			txt, err := deparseExpr(cd.Check)
			if err != nil {
				return nil, err
			}
			cm.Check = txt
		}
		if cd.PrimaryKey {
			if sch.PKIndex != -1 {
				return nil, newExecError("42P16", "multiple primary keys for table %q are not allowed", table)
			}
			sch.PKIndex = len(sch.Cols)
		}
		// Inline UNIQUE (non-PK): back it with an implicit unique index so the
		// existing INSERT/UPDATE uniqueness enforcement applies automatically.
		if cd.Unique && !cd.PrimaryKey {
			uniqueIdx = append(uniqueIdx, indexDef{
				Name: table + "_" + cd.ColName + "_key", Table: table,
				Columns: []string{cd.ColName}, Unique: true,
			})
		}
		sch.Cols = append(sch.Cols, cm)
	}

	raw, err := json.Marshal(&sch)
	if err != nil {
		return nil, newExecError("XX000", "encode schema: %v", err)
	}
	key := e.store.Encoder().SchemaKey(sess.Namespace(), table)
	e.txn.Buffer(txn, transactions.Mutation{Key: key, Value: raw})

	// Persist implicit unique indexes (table is new, so no back-fill needed).
	if len(uniqueIdx) > 0 {
		idxRaw, err := json.Marshal(uniqueIdx)
		if err != nil {
			return nil, newExecError("XX000", "encode index metadata: %v", err)
		}
		e.txn.Buffer(txn, transactions.Mutation{Key: e.indexListKey(sess, table), Value: idxRaw})
	}
	if err := e.commitTx(ctx, txn, owns); err != nil {
		return nil, err
	}
	return &Result{Command: "CREATE TABLE"}, nil
}
