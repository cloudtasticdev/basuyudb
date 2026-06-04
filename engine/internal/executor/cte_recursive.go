package executor

// cte_recursive.go — Task 5: WITH RECURSIVE.
// Extends bindCTEs (in cte.go) to support iterative recursive CTEs.
// A recursive CTE has the form:
//
//   WITH RECURSIVE name AS (
//     base_query          -- anchor member
//     UNION [ALL]
//     recursive_query     -- references name
//   )
//
// The right-hand side (Rarg) of the UNION references the CTE by name, and
// gets re-evaluated with the CTE bound to the current accumulated result set
// until no new rows are produced (fixpoint).

import (
	"context"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
)

// bindCTEsRecursive materialises each CTE in a WITH RECURSIVE clause.
// It is called instead of bindCTEs when wc.Recursive is true.
func (e *execImpl) bindCTEsRecursive(ctx context.Context, sess *session.Session, wc *ast.WithClause, params []Datum) (context.Context, error) {
	for _, c := range wc.CTEs {
		sel, ok := c.Query.(*ast.SelectStmt)
		if !ok {
			return ctx, newExecError("0A000", "CTE %q must be a SELECT", c.Name)
		}

		var ent *cteEntry
		var err error

		// A recursive CTE's SELECT is a UNION with two sides.
		if sel.SetOp == ast.SetOpUnion && sel.Larg != nil && sel.Rarg != nil {
			ent, err = e.evalRecursiveCTE(ctx, c.Name, c.Cols, sel, sess, params)
		} else {
			// RECURSIVE keyword but no self-reference: treat as a regular CTE.
			res, err2 := e.execSelect(ctx, sel, sess, params)
			if err2 != nil {
				return ctx, err2
			}
			sch := &tableSchema{Name: c.Name, PKIndex: -1}
			for _, col := range res.Columns {
				sch.Cols = append(sch.Cols, colMeta{Name: col.Name, TypeOID: col.TypeOID})
			}
			applyCTEColAliases(sch, c.Cols)
			ent = &cteEntry{schema: sch, rows: res.Rows}
		}
		if err != nil {
			return ctx, err
		}

		ctx = withCTE(ctx, c.Name, ent)
	}
	return ctx, nil
}

// evalRecursiveCTE implements the iterative fixpoint evaluation for a UNION
// (or UNION ALL) recursive CTE whose SELECT node has Larg (base) and Rarg
// (recursive step).
func (e *execImpl) evalRecursiveCTE(
	ctx context.Context,
	name string,
	cols []string,
	sel *ast.SelectStmt,
	sess *session.Session,
	params []Datum,
) (*cteEntry, error) {
	// Step 1: evaluate the anchor (base) member.
	baseResult, err := e.execSelect(ctx, sel.Larg, sess, params)
	if err != nil {
		return nil, err
	}

	// Derive the schema from the base result, applying the WITH name(cols)
	// aliases so the recursive member and outer query can reference them.
	sch := &tableSchema{Name: name, PKIndex: -1}
	for _, col := range baseResult.Columns {
		sch.Cols = append(sch.Cols, colMeta{Name: col.Name, TypeOID: col.TypeOID})
	}
	applyCTEColAliases(sch, cols)

	// PostgreSQL recursive-CTE semantics: the recursive member sees ONLY the
	// rows produced by the PREVIOUS iteration (the "working table"), never the
	// full accumulated result. Binding the whole accumulated set re-processes
	// old rows every round and never terminates for UNION ALL.
	result := append([][]Datum(nil), baseResult.Rows...)
	working := baseResult.Rows

	// For UNION (DISTINCT), track which rows have already been emitted so each
	// distinct tuple is produced at most once and the fixpoint is reached.
	seen := map[string]bool{}
	if !sel.All {
		for _, r := range result {
			seen[rowKey(r)] = true
		}
	}

	const maxIterations = 100_000 // safety limit against pathological recursion
	for i := 0; i < maxIterations; i++ {
		if len(working) == 0 {
			break // fixpoint reached
		}
		// Bind the CTE name to the working table (previous iteration's output).
		iterCtx := withCTE(ctx, name, &cteEntry{schema: sch, rows: working})

		iterResult, err := e.execSelect(iterCtx, sel.Rarg, sess, params)
		if err != nil {
			return nil, err
		}

		if sel.All {
			// UNION ALL: every produced row is kept and feeds the next round.
			result = append(result, iterResult.Rows...)
			working = iterResult.Rows
		} else {
			// UNION (DISTINCT): keep only tuples not seen before; those become
			// the next working table. When none are new, the fixpoint is reached.
			var fresh [][]Datum
			for _, r := range iterResult.Rows {
				k := rowKey(r)
				if seen[k] {
					continue
				}
				seen[k] = true
				fresh = append(fresh, r)
			}
			if len(fresh) == 0 {
				break
			}
			result = append(result, fresh...)
			working = fresh
		}
	}

	return &cteEntry{schema: sch, rows: result}, nil
}
