package executor

// explain.go — Task 6: EXPLAIN [ANALYZE] stub.
// Returns a fake query-plan text row (BasuyuDB has no cost-based planner yet).

import (
	"context"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
)

// execExplain handles EXPLAIN [ANALYZE] [VERBOSE] stmt.
// A real cost-based planner is not yet implemented, so this returns a
// placeholder plan text that is structurally valid for PostgreSQL clients.
func (e *execImpl) execExplain(ctx context.Context, s *ast.ExplainStmt, sess *session.Session, params []Datum) (*Result, error) {
	plan := "Seq Scan  (cost=0.00..0.00 rows=0 width=0)"
	if s.Analyze {
		plan = "Seq Scan  (cost=0.00..0.00 rows=0 width=0) (actual time=0.000..0.001 rows=0 loops=1)"
	}
	if s.Verbose {
		plan += "\n  Output: *"
	}
	return &Result{
		Columns:  []Column{{Name: "QUERY PLAN", TypeOID: OIDText}},
		Rows:     [][]Datum{{{Text: plan}}},
		Command:  "EXPLAIN",
	}, nil
}
