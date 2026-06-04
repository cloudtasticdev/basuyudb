// Package executor is the BadgerDB-native SQL execution layer. It consumes the
// canonical ast.Node (from engine/internal/parser) and a *session.Session, and
// runs statements against the managed-mode storage.Store. It does NOT import
// engine/internal/wire — the wire layer depends on executor, never the reverse.
// (by design)
//
// Milestone-1 scope (Mode D Sprint Cluster 2): FROM-less SELECT of constant and
// arithmetic expressions (Gate 1: `SELECT 1`). Table scans, JOINs, DML against
// storage, and the OTel JOIN (Gate 3) are layered on in subsequent milestones
// against this same interface and the same managed Store.
package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/branch"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// PG type OIDs used by the milestone-1 result encoder.
const (
	OIDBool uint32 = 16
	OIDBytea uint32 = 17
	OIDChar uint32 = 18  // "char" — single-byte internal type (pg_type.typtype etc.)
	OIDInt8 uint32 = 20
	OIDInt2 uint32 = 21
	OIDInt4 uint32 = 23
	OIDText uint32 = 25
	OIDOid uint32 = 26  // the oid type itself
	OIDJSON uint32 = 114
	OIDFloat4 uint32 = 700
	OIDFloat8 uint32 = 701
	OIDVarchar uint32 = 1043
	OIDBpchar uint32 = 1042
	OIDDate uint32 = 1082
	OIDTime uint32 = 1083
	OIDTimestamp uint32 = 1114
	OIDTimestamptz uint32 = 1184
	OIDNumeric uint32 = 1700
	OIDUUID uint32 = 2950
	OIDRecord uint32 = 2249 // anonymous composite / ROW(...) value
	OIDJSONB uint32 = 3802
	OIDUnknown  uint32 = 705
	OIDTextArr  uint32 = 1009 // text[] — array of text values
	OIDInterval uint32 = 1186 // interval
	OIDInt4Arr  uint32 = 1007 // int4[]
	OIDInt8Arr  uint32 = 1016 // int8[]
	OIDBoolArr  uint32 = 1000 // bool[]
	OIDFloat8Arr uint32 = 1022 // float8[]
)

// Column describes a result column (name + PostgreSQL type OID).
type Column struct {
	Name string
	TypeOID uint32
}

// Datum is a single result cell in PostgreSQL text format. Null cells carry the
// SQL NULL (encoded as a -1 length DataRow value by the wire layer).
type Datum struct {
	Null bool
	Text string
}

// Result is the outcome of executing one statement.
type Result struct {
	Columns []Column
	Rows [][]Datum
	Command string // tag for CommandComplete: "SELECT", "INSERT", "CREATE TABLE", ...
	RowsAffected int
}

// ExecError carries a PostgreSQL SQLSTATE so the wire layer returns a faithful
// PG ErrorResponse.
type ExecError struct {
	Msg string
	SQLSTATE string
}

func (e *ExecError) Error() string { return fmt.Sprintf("%s (SQLSTATE %s)", e.Msg, e.SQLSTATE) }

func newExecError(sqlstate, format string, a ...interface{}) *ExecError {
	return &ExecError{Msg: fmt.Sprintf(format, a...), SQLSTATE: sqlstate}
}

// Executor runs canonical AST statements against managed storage.
type Executor interface {
	// Execute runs stmt within sess and returns its Result. params are the
	// bound parameter values (PG text format) for $1..$N, or nil for none. If
	// ctx carries an explicit transaction (CtxWithTxn), the statement runs
	// within it (no per-statement commit); otherwise it autocommits.
	Execute(ctx context.Context, stmt ast.Node, sess *session.Session, params []Datum) (*Result, error)

	// BeginExplicit / CommitExplicit / RollbackExplicit drive a multi-statement
	// transaction for the wire layer's BEGIN / COMMIT / ROLLBACK.
	BeginExplicit(ctx context.Context, sess *session.Session) (*transactions.Txn, error)
	// CommitExplicit runs any pending DEFERRABLE constraint checks for tx and, if
	// they pass, commits. A deferred FK violation (23503) aborts the commit. sess
	// supplies the namespace/branch the deferred checks run against.
	CommitExplicit(ctx context.Context, tx *transactions.Txn, sess *session.Session) error
	RollbackExplicit(ctx context.Context, tx *transactions.Txn) error

	// SetConstraints implements SET CONSTRAINTS { ALL | name [, ...] }
	// { DEFERRED | IMMEDIATE } for the explicit transaction tx (the wire layer's
	// active BEGIN block; nil in autocommit, where it is a no-op). Setting a
	// DEFERRABLE constraint IMMEDIATE re-checks any of its pending deferred
	// violations and returns the FK error (23503) if one now fails.
	SetConstraints(ctx context.Context, tx *transactions.Txn, sess *session.Session, all bool, names []string, deferred bool) error

	// SweepOTelRetention deletes otel_spans older than cutoff (RFC3339), for a
	// background retention job. Returns the number removed.
	SweepOTelRetention(ctx context.Context, sess *session.Session, cutoff string) (int, error)

	// CopyTo returns the rows a COPY ... TO STDOUT should stream (the table scan
	// or the embedded query).
	CopyTo(ctx context.Context, sess *session.Session, c *ast.CopyStmt) (*Result, error)

	// CopyFrom bulk-inserts rows received from a COPY ... FROM STDIN stream,
	// honoring defaults and constraints. Returns the number of rows loaded.
	CopyFrom(ctx context.Context, sess *session.Session, table string, columns []string, rows [][]Datum) (int64, error)

	// CopyTargetOIDs returns the type OIDs of the COPY target columns (or all
	// table columns when columns is empty), used to decode binary COPY FROM.
	CopyTargetOIDs(ctx context.Context, sess *session.Session, table string, columns []string) ([]uint32, error)

	// DescribeReturning returns the result columns of an INSERT/UPDATE/DELETE
	// ... RETURNING prepared statement, resolved from the table schema WITHOUT
	// executing (no rows are mutated). ok is false when stmt has no RETURNING.
	// Needed by the extended-protocol Describe path, where executing to learn
	// the columns would be a side effect.
	DescribeReturning(ctx context.Context, stmt ast.Node, sess *session.Session) (cols []Column, ok bool, err error)

	// InferColumns derives the result-column descriptors of a SELECT statement
	// purely from the schema, without executing it. Used by the Describe path when
	// normal execution fails (e.g. unsupported WHERE syntax like = ANY($1)).
	InferColumns(ctx context.Context, sess *session.Session, sel *ast.SelectStmt) []Column
}

// New constructs the canonical executor over a managed Store and the
// single-node transaction engine. (At Gate 4 the same TransactionEngine
// interface is backed by the Raft-replicated commit path.)
func New(store storage.Store, txn transactions.TransactionEngine) Executor {
	return &execImpl{store: store, txn: txn, branches: branch.NewManager(store, txn)}
}

type execImpl struct {
	store storage.Store
	txn transactions.TransactionEngine
	branches *branch.Manager
	// deferred holds per-transaction DEFERRABLE constraint state (SET CONSTRAINTS
	// overrides and pending deferred FK checks), keyed by the explicit
	// *transactions.Txn pointer. See deferred.go.
	deferred deferredRegistry
}

func (e *execImpl) Execute(ctx context.Context, stmt ast.Node, sess *session.Session, params []Datum) (*Result, error) {
	if stmt == nil {
		return nil, newExecError("XX000", "nil statement")
	}
	// Autocommit statements retry on a write-write conflict (first-committer-wins
	// snapshot isolation), matching PostgreSQL Read Committed where a concurrent
	// update is transparently re-applied rather than surfaced. Statements inside
	// an explicit transaction do not retry here — the conflict surfaces at COMMIT
	// as SQLSTATE 40001 for the client to retry the whole transaction.
	if txnFromCtx(ctx) != nil {
		return e.executeOnce(ctx, stmt, sess, params)
	}
	const maxRetries = 64
	for attempt := 0; ; attempt++ {
		res, err := e.executeOnce(ctx, stmt, sess, params)
		if err != nil && errors.Is(err, transactions.ErrWriteConflict) {
			if attempt < maxRetries {
				continue
			}
			return nil, newExecError("40001", "could not serialize access due to concurrent update")
		}
		return res, err
	}
}

func (e *execImpl) executeOnce(ctx context.Context, stmt ast.Node, sess *session.Session, params []Datum) (*Result, error) {
	switch s := stmt.(type) {
	case *ast.SelectStmt:
		return e.execSelect(ctx, s, sess, params)
	case *ast.CreateStmt:
		if s.AsSelect != nil {
			return e.execCreateTableAs(ctx, s, sess, params)
		}
		return e.execCreateTable(ctx, s, sess)
	case *ast.CreateViewStmt:
		return e.execCreateView(ctx, s, sess)
	case *ast.IndexStmt:
		return e.execCreateIndex(ctx, s, sess)
	case *ast.DropStmt:
		if s.IsView {
			return e.execDropView(ctx, s, sess)
		}
		if s.IsSequence {
			return e.execDropSequence(ctx, s, sess)
		}
		if s.IsSchema || s.IsExtension || s.IsType {
			return &Result{Command: "DROP"}, nil
		}
		return e.execDropTable(ctx, s, sess)
	case *ast.TruncateStmt:
		return e.execTruncate(ctx, s, sess)
	case *ast.ShowStmt:
		return e.execShow(ctx, s, sess)
	case *ast.AlterTableStmt:
		return e.execAlterTable(ctx, s, sess)
	case *ast.CreatePolicyStmt:
		return e.execCreatePolicy(ctx, s, sess)
	case *ast.AlterPolicyStmt:
		return e.execAlterPolicy(ctx, s, sess)
	case *ast.DropPolicyStmt:
		return e.execDropPolicy(ctx, s, sess)
	case *ast.InsertStmt:
		return e.execInsert(ctx, s, sess, params)
	case *ast.UpdateStmt:
		return e.execUpdate(ctx, s, sess, params)
	case *ast.DeleteStmt:
		return e.execDelete(ctx, s, sess, params)
	case *ast.CreateBranchStmt:
		return e.execCreateBranch(ctx, s, sess)
	case *ast.MergeBranchStmt:
		return e.execMergeBranch(ctx, s, sess)
	case *ast.DropBranchStmt:
		return e.execDropBranch(ctx, s, sess)
	case *ast.SavepointStmt:
		if s.Release {
			return &Result{Command: "RELEASE"}, nil
		}
		if s.Rollback {
			return &Result{Command: "ROLLBACK"}, nil
		}
		if s.Name == "vacuum" {
			return &Result{Command: "VACUUM"}, nil
		}
		if s.Name == "analyze" {
			return &Result{Command: "ANALYZE"}, nil
		}
		if s.Name == "reindex" {
			return &Result{Command: "REINDEX"}, nil
		}
		return &Result{Command: "SAVEPOINT"}, nil
	case *ast.CreateSeqStmt:
		return e.execCreateSequence(ctx, s, sess)
	case *ast.CreateSchemaStmt:
		return &Result{Command: "CREATE SCHEMA"}, nil
	case *ast.CreateExtensionStmt:
		return &Result{Command: "CREATE EXTENSION"}, nil
	case *ast.CreateEnumStmt:
		return e.execCreateEnum(ctx, s, sess)
	case *ast.CreateTypeStmt:
		return e.execCreateType(ctx, s, sess)
	case *ast.ListenStmt:
		if s.IsNotify {
			return &Result{Command: "NOTIFY"}, nil
		}
		if s.IsUnlisten {
			return &Result{Command: "UNLISTEN"}, nil
		}
		return &Result{Command: "LISTEN"}, nil
	case *ast.DoNothingStmt:
		return &Result{Command: "OK"}, nil
	case *ast.ExplainStmt:
		return e.execExplain(ctx, s, sess, params)
	default:
		return nil, newExecError("0A000", "unsupported statement type %T", stmt)
	}
}

// execSelect handles SELECT. A FROM-less SELECT evaluates constant/expression
// targets (Gate 1). A SELECT with a single-table FROM performs a table scan
// (milestone-3); JOINs are added at Gate 3.
func (e *execImpl) execSelect(ctx context.Context, s *ast.SelectStmt, sess *session.Session, params []Datum) (*Result, error) {
	// WITH: materialize CTEs and carry them in ctx for FROM resolution.
	if s.WithClause != nil {
		nctx, err := e.bindCTEs(ctx, sess, s.WithClause, params)
		if err != nil {
			return nil, err
		}
		ctx = nctx
	}
	if s.SetOp != ast.SetOpNone {
		return e.execSetOp(ctx, s, sess, params)
	}
	if len(s.FromClause) > 0 {
		return e.execSelectFrom(ctx, s, sess, params)
	}

	// FROM-less SELECT: still allow scalar / EXISTS subqueries in the target list.
	// sess is threaded so current_setting / set_config / current_user resolve
	// against the session (e.g. SELECT set_config('app.tenant', 't1', false)).
	ev := &evaluator{params: params, sess: sess, lookupComposite: e.compositeResolverCtx(ctx, sess), runSub: func(sel *ast.SelectStmt) (*Result, error) {
		return e.execSelect(ctx, sel, sess, params)
	}}

	// Set-returning function in the SELECT list (e.g. SELECT unnest(ARRAY[1,2,3]))
	// expands to one row per element. Handle the common single-SRF-target case.
	if srf, idx := singleSRFTarget(s.TargetList); srf != nil {
		elems, _, err := ev.srfElements(srf)
		if err != nil {
			return nil, err
		}
		name := s.TargetList[idx].Name
		if name == "" {
			name = defaultColName(srf, idx)
		}
		colOID := OIDText
		if len(elems) > 0 {
			colOID = elems[0].oid
		}
		outRows := make([][]Datum, 0, len(elems))
		for _, el := range elems {
			outRows = append(outRows, []Datum{{Null: el.null, Text: el.text}})
		}
		return &Result{
			Columns: []Column{{Name: name, TypeOID: colOID}},
			Rows:    outRows,
			Command: "SELECT",
		}, nil
	}

	cols := make([]Column, 0, len(s.TargetList))
	row := make([]Datum, 0, len(s.TargetList))

	for i, tgt := range s.TargetList {
		v, err := ev.eval(tgt.Val)
		if err != nil {
			return nil, err
		}
		name := tgt.Name
		if name == "" {
			name = defaultColName(tgt.Val, i)
		}
		cols = append(cols, Column{Name: name, TypeOID: v.oid})
		row = append(row, Datum{Null: v.null, Text: v.text})
	}

	return &Result{
		Columns: cols,
		Rows: [][]Datum{row},
		Command: "SELECT",
	}, nil
}

// singleSRFTarget returns the lone set-returning FuncCall target and its index
// when the target list is exactly one SRF call; otherwise nil. This is the
// common app pattern (SELECT unnest(...), SELECT jsonb_array_elements(...)).
func singleSRFTarget(targets []*ast.ResTarget) (*ast.FuncCall, int) {
	if len(targets) != 1 {
		return nil, -1
	}
	if fc, ok := targets[0].Val.(*ast.FuncCall); ok && fc.Over == nil {
		name := strings.ToLower(strings.Join(fc.FuncName, "."))
		if isSRFName(name) {
			return fc, 0
		}
	}
	return nil, -1
}

// defaultColName mirrors PostgreSQL's unnamed-column behaviour: a bare column
// reference uses the column name; a function call uses the function name;
// everything else is "?column?".
func defaultColName(n ast.Node, idx int) string {
	switch v := n.(type) {
	case *ast.ColumnRef:
		if len(v.Fields) > 0 {
			return v.Fields[len(v.Fields)-1]
		}
	case *ast.FuncCall:
		if len(v.FuncName) > 0 {
			return v.FuncName[len(v.FuncName)-1]
		}
	}
	return "?column?"
}
