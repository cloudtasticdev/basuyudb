package executor

import (
	"context"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
)

// SweepOTelRetention deletes otel_spans whose started_at is older than cutoff
// (an RFC3339 timestamp string; lexicographic order is chronological for UTC
// RFC3339). It returns the number of spans removed. Intended to be called
// periodically by a retention job. (OTel span TTL — PRD-010.)
func (e *execImpl) SweepOTelRetention(ctx context.Context, sess *session.Session, cutoff string) (int, error) {
	stmt := &ast.DeleteStmt{
		Relation: &ast.RangeVar{RelName: OTelSpansTable},
		WhereClause: &ast.A_Expr{
			Kind:  ast.AEXPR_OP,
			Name:  "<",
			Lexpr: &ast.ColumnRef{Fields: []string{"started_at"}},
			Rexpr: &ast.A_Const{Type: ast.ConstString, Val: cutoff},
		},
	}
	res, err := e.execDelete(ctx, stmt, sess, nil)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected, nil
}
