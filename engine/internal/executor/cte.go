package executor

import (
	"context"
	"strings"

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
// later ones) and returns a context carrying them. Both recursive and
// non-recursive CTEs are handled.
func (e *execImpl) bindCTEs(ctx context.Context, sess *session.Session, wc *ast.WithClause, params []Datum) (context.Context, error) {
	if wc.Recursive {
		return e.bindCTEsRecursive(ctx, sess, wc, params)
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
		applyCTEColAliases(sch, c.Cols)
		ctx = withCTE(ctx, c.Name, &cteEntry{schema: sch, rows: res.Rows})
	}
	return ctx, nil
}

// applyCTEColAliases renames the CTE's output columns to the explicit column
// list from `WITH name(col1, col2, ...) AS (...)`. PostgreSQL requires the list
// length to be <= the query's output arity; extra query columns keep their
// inferred names. This is what makes `WITH RECURSIVE t(n) AS (SELECT 1 ...)`
// expose `n` to the recursive member and the outer query.
func applyCTEColAliases(sch *tableSchema, cols []string) {
	for i := 0; i < len(cols) && i < len(sch.Cols); i++ {
		if cols[i] != "" {
			sch.Cols[i].Name = cols[i]
		}
	}
}

// rowKey returns a stable string signature of a row for set membership
// (UNION DISTINCT dedup in recursive CTEs).
func rowKey(r []Datum) string {
	var b strings.Builder
	for _, d := range r {
		if d.Null {
			b.WriteString("\x00N\x00")
		} else {
			b.WriteString(d.Text)
		}
		b.WriteByte(0x1f)
	}
	return b.String()
}
