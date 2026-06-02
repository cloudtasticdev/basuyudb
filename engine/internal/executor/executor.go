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
	"fmt"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/branch"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// PG type OIDs used by the milestone-1 result encoder.
const (
	OIDBool uint32 = 16
	OIDInt8 uint32 = 20
	OIDInt4 uint32 = 23
	OIDText uint32 = 25
	OIDFloat8 uint32 = 701
	OIDUnknown uint32 = 705
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
	// bound parameter values (PG text format) for $1..$N, or nil for none.
	Execute(ctx context.Context, stmt ast.Node, sess *session.Session, params []Datum) (*Result, error)
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
}

func (e *execImpl) Execute(ctx context.Context, stmt ast.Node, sess *session.Session, params []Datum) (*Result, error) {
	if stmt == nil {
		return nil, newExecError("XX000", "nil statement")
	}
	switch s := stmt.(type) {
	case *ast.SelectStmt:
		return e.execSelect(ctx, s, sess, params)
	case *ast.CreateStmt:
		return e.execCreateTable(ctx, s, sess)
	case *ast.IndexStmt:
		return e.execCreateIndex(ctx, s, sess)
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
	default:
		return nil, newExecError("0A000", "unsupported statement type %T", stmt)
	}
}

// execSelect handles SELECT. A FROM-less SELECT evaluates constant/expression
// targets (Gate 1). A SELECT with a single-table FROM performs a table scan
// (milestone-3); JOINs are added at Gate 3.
func (e *execImpl) execSelect(ctx context.Context, s *ast.SelectStmt, sess *session.Session, params []Datum) (*Result, error) {
	if len(s.FromClause) > 0 {
		return e.execSelectFrom(ctx, s, sess, params)
	}

	ev := &evaluator{params: params}
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
