package executor

import (
	"context"
	"errors"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/branch"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
)

// execCreateBranch creates a feature branch (O(1) metadata write — the basis of
// the <500ms Gate-2 acceptance).
func (e *execImpl) execCreateBranch(ctx context.Context, s *ast.CreateBranchStmt, sess *session.Session) (*Result, error) {
	err := e.branches.Create(ctx, sess.Auth, s.BranchName, s.FromBranch)
	switch {
	case errors.Is(err, branch.ErrBranchExists):
		return nil, newExecError("42P07", "branch %q already exists", s.BranchName)
	case errors.Is(err, branch.ErrCannotModifyMain):
		return nil, newExecError("42501", "the main branch cannot be created")
	case err != nil:
		return nil, newExecError("XX000", "create branch: %v", err)
	}
	return &Result{Command: "CREATE BRANCH"}, nil
}

// execMergeBranch merges a branch's schema + row data into its parent with
// conflict detection (by design).
func (e *execImpl) execMergeBranch(ctx context.Context, s *ast.MergeBranchStmt, sess *session.Session) (*Result, error) {
	res, err := e.branches.Merge(ctx, sess.Auth, s.SourceBranch)
	switch {
	case errors.Is(err, branch.ErrNoSuchBranch):
		return nil, newExecError("42P01", "branch %q does not exist", s.SourceBranch)
	case errors.Is(err, branch.ErrCannotModifyMain):
		return nil, newExecError("42501", "cannot merge the main branch")
	case err != nil && res != nil && len(res.Conflicts) > 0:
		return nil, newExecError("40001", "merge of %q has %d conflict(s); resolve via basuyudb_conflicts()", s.SourceBranch, len(res.Conflicts))
	case err != nil:
		return nil, newExecError("XX000", "merge branch: %v", err)
	}
	return &Result{Command: "MERGE BRANCH", RowsAffected: res.RowsMerged + res.RowsDeleted}, nil
}

// execDropBranch removes a branch and all its data.
func (e *execImpl) execDropBranch(ctx context.Context, s *ast.DropBranchStmt, sess *session.Session) (*Result, error) {
	err := e.branches.Drop(ctx, sess.Auth, s.BranchName)
	switch {
	case errors.Is(err, branch.ErrNoSuchBranch):
		return nil, newExecError("42P01", "branch %q does not exist", s.BranchName)
	case errors.Is(err, branch.ErrCannotModifyMain):
		return nil, newExecError("42501", "the main branch cannot be dropped")
	case err != nil:
		return nil, newExecError("XX000", "drop branch: %v", err)
	}
	return &Result{Command: "DROP BRANCH"}, nil
}
