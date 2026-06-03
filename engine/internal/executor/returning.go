package executor

import (
	"context"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
)

// DescribeReturning resolves the RETURNING result columns of an INSERT/UPDATE/
// DELETE statement from the table schema, without executing (no mutation). Bare
// column references adopt their real schema OID so drivers negotiate the correct
// result format (e.g. RETURNING id on a SERIAL key is int4, not text).
func (e *execImpl) DescribeReturning(ctx context.Context, stmt ast.Node, sess *session.Session) ([]Column, bool, error) {
	var table string
	var returning []*ast.ResTarget
	switch s := stmt.(type) {
	case *ast.InsertStmt:
		table, returning = s.Relation.RelName, s.ReturningList
	case *ast.UpdateStmt:
		table, returning = s.Relation.RelName, s.ReturningList
	case *ast.DeleteStmt:
		table, returning = s.Relation.RelName, s.ReturningList
	default:
		return nil, false, nil
	}
	if len(returning) == 0 {
		return nil, false, nil
	}

	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, false, err
	}
	defer e.rollbackTx(ctx, txn, owns)
	sch, err := e.resolveSchema(ctx, txn, sess, table)
	if err != nil {
		return nil, false, err
	}

	var cols []Column
	for i, t := range returning {
		if _, ok := t.Val.(*ast.A_Star); ok {
			for _, c := range sch.Cols {
				cols = append(cols, Column{Name: c.Name, TypeOID: c.TypeOID})
			}
			continue
		}
		name := t.Name
		oid := uint32(OIDText)
		if cr, ok := t.Val.(*ast.ColumnRef); ok && len(cr.Fields) > 0 {
			if idx := sch.colIndex(cr.Fields[len(cr.Fields)-1]); idx >= 0 {
				oid = sch.Cols[idx].TypeOID
				if name == "" {
					name = sch.Cols[idx].Name
				}
			}
		}
		if name == "" {
			name = defaultColName(t.Val, i)
		}
		cols = append(cols, Column{Name: name, TypeOID: oid})
	}
	return cols, true, nil
}

// projectReturning evaluates a RETURNING target list over the affected rows
// (each a full cell slice in schema column order) and builds the result rows.
// RETURNING * expands to all columns.
func projectReturning(sch *tableSchema, table string, returning []*ast.ResTarget, rows [][]Datum, params []Datum) (*Result, error) {
	star := len(returning) == 1
	if star {
		_, star = returning[0].Val.(*ast.A_Star)
	}

	var cols []Column
	out := make([][]Datum, 0, len(rows))
	for _, cells := range rows {
		if star {
			if cols == nil {
				for _, c := range sch.Cols {
					cols = append(cols, Column{Name: c.Name, TypeOID: c.TypeOID})
				}
			}
			out = append(out, append([]Datum(nil), cells...))
			continue
		}
		ev := &evaluator{params: params, resolveCol: rowResolver(sch, table, cells)}
		rowOut := make([]Datum, len(returning))
		rowCols := make([]Column, len(returning))
		for i, t := range returning {
			v, err := ev.eval(t.Val)
			if err != nil {
				return nil, err
			}
			rowOut[i] = Datum{Null: v.null, Text: v.text}
			name := t.Name
			if name == "" {
				name = defaultColName(t.Val, i)
			}
			rowCols[i] = Column{Name: name, TypeOID: v.oid}
		}
		if cols == nil {
			cols = rowCols
		}
		out = append(out, rowOut)
	}
	if cols == nil { // RETURNING with zero affected rows: derive column names.
		for i, t := range returning {
			if _, ok := t.Val.(*ast.A_Star); ok {
				for _, c := range sch.Cols {
					cols = append(cols, Column{Name: c.Name, TypeOID: c.TypeOID})
				}
				break
			}
			name := t.Name
			if name == "" {
				name = defaultColName(t.Val, i)
			}
			cols = append(cols, Column{Name: name, TypeOID: OIDText})
		}
	}
	return &Result{Columns: cols, Rows: out}, nil
}
