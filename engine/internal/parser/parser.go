package parser

import (
	"fmt"
	"sync"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
)

const (
	maxSQLLength = 1 << 20 // 1 MiB hard cap ()
)

// parserPool reuses yyParserImpl instances across Parse calls. The generated
// yyParse() allocates a fresh *yyParserImpl every call — and that struct embeds
// the parse value-stack inline ([yyInitialStackSize]yySymType), the single
// largest allocation of a typical parse. Pooling reuses that stack, which every
// query (simple-query AND extended-protocol Parse) pays for. The Parse method
// fully re-initializes parser state (yystate=0, char=-1, stack pointer reset)
// on entry, so a reused instance is safe.
var parserPool = sync.Pool{New: func() any { return &yyParserImpl{} }}

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
	// Pooled parser: reuse the value-stack instead of allocating it per call.
	// On a panic inside Parse, the instance is simply not returned to the pool
	// (the outer deferred recover converts it to a ParseError) — acceptable.
	p := parserPool.Get().(*yyParserImpl)
	p.Parse(lex)
	parserPool.Put(p)

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
