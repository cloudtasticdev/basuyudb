package executor

import (
	"context"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
)

// cteCtxKey is the context key under which materialized CTEs are carried so the
// FROM-resolution path (materialize / planIndexScan) can serve them like tables.
type cteCtxKey struct{}

// cteEntry is a materialized common table expression: a synthetic schema (column
// names/types) plus the already-evaluated rows.
type cteEntry struct {
	schema *tableSchema
	rows [][]Datum
}

type cteMap map[string]*cteEntry

func ctesFrom(ctx context.Context) cteMap {
	m, _ := ctx.Value(cteCtxKey{}).(cteMap)
	return m
}

// withCTE returns a child context that adds one named CTE to any already in
// scope (a copy, so sibling scopes don't leak into each other).
func withCTE(ctx context.Context, name string, ent *cteEntry) context.Context {
	prev := ctesFrom(ctx)
	m := make(cteMap, len(prev)+1)
	for k, v := range prev {
		m[k] = v
	}
	m[name] = ent
	return context.WithValue(ctx, cteCtxKey{}, m)
}

// lookupCTE returns a materialized CTE by name, if one is in scope.
func lookupCTE(ctx context.Context, name string) (*cteEntry, bool) {
	m := ctesFrom(ctx)
	if m == nil {
		return nil, false
	}
	ent, ok := m[name]
	return ent, ok
}

// bindCTEs materializes each CTE in a WITH clause (earlier CTEs are visible to
// later ones) and returns a context carrying them. Non-recursive only.
func (e *execImpl) bindCTEs(ctx context.Context, sess *session.Session, wc *ast.WithClause, params []Datum) (context.Context, error) {
	if wc.Recursive {
		return ctx, newExecError("0A000", "WITH RECURSIVE is not supported yet")
	}
	for _, c := range wc.CTEs {
		sel, ok := c.Query.(*ast.SelectStmt)
		if !ok {
			return ctx, newExecError("0A000", "CTE %q must be a SELECT", c.Name)
		}
		res, err := e.execSelect(ctx, sel, sess, params)
		if err != nil {
			return ctx, err
		}
		sch := &tableSchema{Name: c.Name, PKIndex: -1}
		for _, col := range res.Columns {
			sch.Cols = append(sch.Cols, colMeta{Name: col.Name, TypeOID: col.TypeOID})
		}
		ctx = withCTE(ctx, c.Name, &cteEntry{schema: sch, rows: res.Rows})
	}
	return ctx, nil
}
