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
	Name     string       `json:"name"`
	TypeOID  uint32       `json:"type_oid"`
	NotNull  bool         `json:"not_null"`
	PK       bool         `json:"pk"`
	Unique   bool         `json:"unique,omitempty"`
	Default  *defaultSpec `json:"default,omitempty"`
	Check    string       `json:"check,omitempty"` // deparsed CHECK expression text
	FKTable  string       `json:"fk_table,omitempty"`
	FKColumn string       `json:"fk_column,omitempty"`
	// FKOnDelete / FKOnUpdate carry the referential action for an inline or
	// ADD-CONSTRAINT foreign key (canonical "CASCADE"/"SET NULL"/...; "" = the
	// default RESTRICT/NO ACTION). Read by enforceFKParent (item 6).
	FKOnDelete string `json:"fk_on_delete,omitempty"`
	FKOnUpdate string `json:"fk_on_update,omitempty"`
	// FKCols, when non-empty, holds the FULL set of (local, referenced) column
	// pairs of a composite foreign key whose first local column is this column.
	// FKTable/FKColumn still carry the first pair for backward compatibility, and
	// the additional pairs (FKCols[1:]) are present only on the anchor column. A
	// single-column FK leaves FKCols empty and is described entirely by
	// FKTable/FKColumn. Composite FK enforcement matches on every pair (item 2).
	FKCols []fkColPair `json:"fk_cols,omitempty"`
	// FKName is the (declared or synthesized) name of the foreign-key constraint
	// anchored on this column, used by SET CONSTRAINTS and deferred-check
	// tracking. FKDeferrable / FKInitiallyDeferred carry the constraint's
	// DEFERRABLE / INITIALLY DEFERRED declaration (PostgreSQL deferred FK
	// checking). A non-deferrable FK is always checked immediately.
	FKName              string `json:"fk_name,omitempty"`
	FKDeferrable        bool   `json:"fk_deferrable,omitempty"`
	FKInitiallyDeferred bool   `json:"fk_initially_deferred,omitempty"`
	// Generated carries GENERATED ALWAYS AS (expr) STORED. GeneratedExpr is the
	// deparsed generation expression, recomputed on INSERT/UPDATE (item 2).
	Generated     bool   `json:"generated,omitempty"`
	GeneratedExpr string `json:"generated_expr,omitempty"`
	// CompositeType, when non-empty, names the registered composite type this
	// column was declared with (its values are stored as record text under
	// OIDText). Enables (compositecol).field decoding (item 3).
	CompositeType string `json:"composite_type,omitempty"`
}

// fkColPair is one (local column, referenced column) pair of a (possibly
// composite) foreign key.
type fkColPair struct {
	Local string `json:"local"`
	Ref   string `json:"ref"`
}

// defaultSpec is a column DEFAULT, classified at CREATE TABLE time into a small
// set of evaluable forms so INSERT can materialize a value without re-parsing.
type defaultSpec struct {
	Kind string `json:"kind"`           // "const" | "now" | "uuid" | "serial"
	Text string `json:"text,omitempty"` // literal text for Kind=="const"
	OID  uint32 `json:"oid,omitempty"`  // value OID for Kind=="const"
	Seq  string `json:"seq,omitempty"`  // sequence name for Kind=="serial"
}

// rlsPolicy is one persisted Row-Level Security policy on a table. Predicate
// expressions are stored as deparsed SQL text (the same approach as CHECK
// constraints) and re-parsed at enforcement time. UsingExpr is the USING
// predicate (row visibility / target selection); CheckExpr is the WITH CHECK
// predicate (new-row validation). An empty expression string means "absent".
type rlsPolicy struct {
	Name       string   `json:"name"`
	Command    string   `json:"command"`    // "ALL" | "SELECT" | "INSERT" | "UPDATE" | "DELETE"
	Permissive bool     `json:"permissive"` // true = PERMISSIVE (OR), false = RESTRICTIVE (AND)
	Roles      []string `json:"roles,omitempty"`
	UsingExpr  string   `json:"using,omitempty"`
	CheckExpr  string   `json:"with_check,omitempty"`
}

// appliesToRole reports whether the policy applies to user. An empty Roles list
// (or one naming PUBLIC) applies to everyone; otherwise the policy applies only
// when user is among the named roles (case-insensitive).
func (p *rlsPolicy) appliesToRole(user string) bool {
	if len(p.Roles) == 0 {
		return true
	}
	for _, r := range p.Roles {
		if strings.EqualFold(r, "public") || strings.EqualFold(r, user) {
			return true
		}
	}
	return false
}

// appliesToCommand reports whether the policy governs the given command. A
// policy's Command is "ALL" (applies to every command) or a specific command.
// For visibility on UPDATE/DELETE, PostgreSQL also applies SELECT-less ALL/cmd
// USING predicates; callers pass the concrete command they are enforcing.
func (p *rlsPolicy) appliesToCommand(cmd string) bool {
	return p.Command == "ALL" || strings.EqualFold(p.Command, cmd)
}

// tableSchema is a persisted table definition stored under SchemaKey.
type tableSchema struct {
	Name    string    `json:"name"`
	Cols    []colMeta `json:"cols"`
	PKIndex int       `json:"pk_index"` // -1 if no primary key
	// RLSEnabled mirrors pg_class.relrowsecurity: when true the table's row
	// security policies are enforced. RLSForced mirrors relforcerowsecurity: when
	// true RLS applies even to the table owner. Policies holds the table's
	// row-security policies. (Row-Level Security, this wave.)
	RLSEnabled bool        `json:"rls_enabled,omitempty"`
	RLSForced  bool        `json:"rls_forced,omitempty"`
	Policies   []rlsPolicy `json:"policies,omitempty"`
}

// findPolicy returns a pointer to the named policy (case-insensitive) and its
// index, or (nil, -1) if absent.
func (s *tableSchema) findPolicy(name string) (*rlsPolicy, int) {
	for i := range s.Policies {
		if strings.EqualFold(s.Policies[i].Name, name) {
			return &s.Policies[i], i
		}
	}
	return nil, -1
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
		if c.IfNotExists {
			return &Result{Command: "CREATE TABLE"}, nil
		}
		return nil, newExecError("42P07", "relation %q already exists", table)
	}

	sch := tableSchema{Name: table, PKIndex: -1, Cols: make([]colMeta, 0, len(c.TableElts))}
	var uniqueIdx []indexDef
	for _, cd := range c.TableElts {
		cm := colMeta{
			Name:    cd.ColName,
			TypeOID: oidForTypeName(cd.TypeName),
			NotNull: cd.NotNull,
			PK:      cd.PrimaryKey,
			Unique:  cd.Unique,
		}
		// A column declared with a registered composite type stores record text
		// (under OIDText); record the type name so (col).field can decode it.
		if _, ok := e.loadComposite(ctx, txn, sess, cd.TypeName); ok {
			cm.CompositeType = cd.TypeName
		}
		// SERIAL/BIGSERIAL/SMALLSERIAL and GENERATED ... AS IDENTITY both become an
		// integer column with an implicit auto-increment sequence default. IDENTITY
		// keeps its declared type's OID (it may be int4/int8); SERIAL maps the
		// pseudo-type to its base integer OID.
		if base, ok := serialBaseOID(cd.TypeName); ok {
			cm.TypeOID = base
			cm.NotNull = true
			cm.Default = &defaultSpec{Kind: "serial", Seq: table + "_" + cd.ColName + "_seq"}
		} else if cd.Identity {
			cm.NotNull = true
			cm.Default = &defaultSpec{Kind: "serial", Seq: table + "_" + cd.ColName + "_seq"}
		} else if cd.Generated {
			// GENERATED ALWAYS AS (expr) STORED: computed from the row on write.
			txt, err := deparseExpr(cd.GeneratedExpr)
			if err != nil {
				return nil, err
			}
			cm.Generated = true
			cm.GeneratedExpr = txt
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
			cm.FKOnDelete = cd.FKOnDelete
			cm.FKOnUpdate = cd.FKOnUpdate
			cm.FKName = cd.FKName
			if cm.FKName == "" {
				cm.FKName = table + "_" + cd.ColName + "_fkey"
			}
			cm.FKDeferrable = cd.FKDeferrable
			cm.FKInitiallyDeferred = cd.FKInitiallyDeferred
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

	// Table-level constraints (PRIMARY KEY / UNIQUE / FOREIGN KEY / CHECK declared
	// in the CREATE TABLE body, e.g. `PRIMARY KEY (a, b)` or
	// `FOREIGN KEY (a, b) REFERENCES t (x, y)`). The table is new and has no rows,
	// so PK/UNIQUE indexes are appended without a back-fill. Shared FK/CHECK
	// recorders keep behavior identical to ALTER TABLE ... ADD CONSTRAINT.
	for _, tc := range c.TableConstraints {
		if tc == nil {
			continue
		}
		switch tc.ConstraintType {
		case ast.ConstrPrimaryKey, ast.ConstrUnique:
			for _, col := range tc.Columns {
				if sch.colIndex(col) < 0 {
					return nil, newExecError("42703", "column %q named in key does not exist", col)
				}
			}
			name := tc.Name
			suffix := "key"
			if tc.ConstraintType == ast.ConstrPrimaryKey {
				suffix = "pkey"
			}
			if name == "" {
				name = table + "_" + strings.Join(tc.Columns, "_") + "_" + suffix
			}
			if tc.ConstraintType == ast.ConstrPrimaryKey {
				if sch.PKIndex != -1 {
					return nil, newExecError("42P16", "multiple primary keys for table %q are not allowed", table)
				}
				for _, col := range tc.Columns {
					ci := sch.colIndex(col)
					sch.Cols[ci].NotNull = true
					sch.Cols[ci].PK = true
				}
				// A single-column table-level PK sets PKIndex (the row-key column);
				// a composite PK leaves PKIndex == -1 and is enforced via its unique
				// index, mirroring the ADD CONSTRAINT path.
				if len(tc.Columns) == 1 {
					sch.PKIndex = sch.colIndex(tc.Columns[0])
				}
			}
			uniqueIdx = append(uniqueIdx, indexDef{
				Name: name, Table: table, Columns: append([]string(nil), tc.Columns...), Unique: true,
			})
		case ast.ConstrForeignKey:
			if err := recordForeignKey(&sch, table, tc); err != nil {
				return nil, err
			}
		case ast.ConstrCheck:
			if err := recordCheck(&sch, tc.Expr); err != nil {
				return nil, err
			}
		}
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
