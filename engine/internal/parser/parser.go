package parser

import (
	"fmt"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
)

const (
	maxSQLLength = 1 << 20 // 1 MiB hard cap ()
)

// Parse parses a single SQL statement into the canonical ast.Node. It never
// panics: any internal panic is recovered and converted to a ParseError with
// SQLSTATE XX000 (the design specs / ).
func Parse(sql string) (node ast.Node, err error) {
	if len(sql) > maxSQLLength {
		return nil, &ParseError{Msg: "statement exceeds maximum length", SQLSTATE: "54000"}
	}

	defer func() {
		if r := recover(); r != nil {
			node = nil
			err = &ParseError{Msg: fmt.Sprintf("internal parser error: %v", r), SQLSTATE: "XX000"}
		}
	}()

	lex := newLexer(sql)
	yyParse(lex)

	if lex.err != nil {
		return nil, lex.err
	}
	if lex.result == nil {
		return nil, &ParseError{Msg: "empty statement", SQLSTATE: "42601"}
	}
	return lex.result, nil
}

// MaxParam returns the highest $N parameter index referenced by a freshly
// parsed statement. Callers needing this must re-lex; for V0.1 the executor
// derives parameter count from the wire Bind message instead.

// colRefFromRangeVar converts a parsed qualified name used in expression
// position into a ColumnRef. RangeVar carries the qualifier in SchemaName and
// the leaf in RelName.
func colRefFromRangeVar(rv *ast.RangeVar) ast.Node {
	if rv.SchemaName != "" {
		return &ast.ColumnRef{Fields: []string{rv.SchemaName, rv.RelName}}
	}
	return &ast.ColumnRef{Fields: []string{rv.RelName}}
}

// valuesToTargets wraps a VALUES expression list as a target list so an
// InsertStmt can carry it via SelectStmt.TargetList.
func valuesToTargets(vals []ast.Node) []*ast.ResTarget {
	out := make([]*ast.ResTarget, 0, len(vals))
	for _, v := range vals {
		out = append(out, &ast.ResTarget{Val: v})
	}
	return out
}
