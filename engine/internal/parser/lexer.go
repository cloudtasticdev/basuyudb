// Package parser is the BasuyuDB SQL parser. The grammar is processed by goyacc
// (ADR-008) and emits the canonical engine/internal/ast node set. This is the
// milestone-1 grammar (Mode D Sprint Cluster 2): SELECT (target list, FROM,
// JOIN, WHERE, GROUP BY, HAVING, ORDER BY, LIMIT/OFFSET), INSERT, UPDATE,
// DELETE, CREATE TABLE, and branch DDL. The milestone-2 grammar (full PG
// gram.y port) extends this without changing the ast contract.
package parser

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
)

// lexer is the hand-written scanner that feeds the goyacc-generated parser. The
// goyacc parser calls Lex to pull tokens and Error on a syntax error.
type lexer struct {
	src string
	pos int
	result ast.Node // set by the grammar's top rule
	err *ParseError // first error encountered
	maxParam int // highest $N seen (for validation)
}

func newLexer(src string) *lexer { return &lexer{src: src} }

// keywords maps upper-cased identifiers to their token constants. Only reserved
// and grammar-significant words appear here; everything else lexes as IDENT.
var keywords = map[string]int{
	"SELECT": SELECT, "FROM": FROM, "WHERE": WHERE, "AS": AS,
	"JOIN": JOIN, "INNER": INNER, "LEFT": LEFT, "RIGHT": RIGHT,
	"FULL": FULL, "CROSS": CROSS, "ON": ON, "USING": USING,
	"AND": AND, "OR": OR, "NOT": NOT, "NULL": NULL, "IS": IS,
	"ORDER": ORDER, "BY": BY, "ASC": ASC, "DESC": DESC,
	"GROUP": GROUP, "HAVING": HAVING, "LIMIT": LIMIT, "OFFSET": OFFSET,
	"INSERT": INSERT, "INTO": INTO, "VALUES": VALUES,
	"UPDATE": UPDATE, "SET": SET, "DELETE": DELETE,
	"CREATE": CREATE, "TABLE": TABLE, "PRIMARY": PRIMARY, "KEY": KEY,
	"INDEX": INDEX, "UNIQUE": UNIQUE,
	"BRANCH": BRANCH, "MERGE": MERGE, "DROP": DROP, "INTO_BRANCH": -1,
	"TRUE": TRUE, "FALSE": FALSE, "DISTINCT": DISTINCT,
	"ALTER": ALTER, "TRUNCATE": TRUNCATE, "COLUMN": COLUMN,
	"ADD": ADD, "IF": IF, "EXISTS": EXISTS, "IN": IN, "RETURNING": RETURNING,
	"CONFLICT": CONFLICT, "DO": DO, "NOTHING": NOTHING, "DEFAULT": DEFAULT,
	"CASE": CASE, "WHEN": WHEN, "THEN": THEN, "ELSE": ELSE, "END": END,
	"UNION": UNION, "INTERSECT": INTERSECT, "EXCEPT": EXCEPT, "ALL": ALL,
	"REFERENCES": REFERENCES, "CHECK": CHECK, "FOREIGN": FOREIGN, "WITH": WITH,
	"OVER": OVER, "PARTITION": PARTITION, "VIEW": VIEW, "REPLACE": REPLACE,
	"COPY": COPY, "TO": TO, "STDIN": STDIN, "STDOUT": STDOUT,
	"FORMAT": FORMAT, "DELIMITER": DELIMITER, "HEADER": HEADER,
	"SHOW": SHOW, "CAST": CAST,
	"SAVEPOINT": SAVEPOINT, "RELEASE": RELEASE,
	"LATERAL": LATERAL,
	"SEQUENCE": SEQUENCE, "EXTENSION": EXTENSION, "SCHEMA": SCHEMA,
	"LISTEN": LISTEN, "NOTIFY": NOTIFY, "UNLISTEN": UNLISTEN,
	"ENUM": ENUM, "TYPE": TYPE,
	"FOR": FOR,
	"SHARE": SHARE, "NOWAIT": NOWAIT, "SKIP": SKIP, "LOCKED": LOCKED,
	"START": START, "INCREMENT": INCREMENT,
	"MINVALUE": MINVALUE, "MAXVALUE": MAXVALUE,
	"CYCLE": CYCLE, "CACHE": CACHE, "OWNED": OWNED,
	"AUTHORIZATION": AUTHORIZATION,
	"NO": NO,
	"VACUUM": VACUUM, "ANALYSE": ANALYSE, "ANALYZE": ANALYSE,
	"REINDEX": REINDEX,
	"ARRAY":      ARRAY,
	"INTERVAL":   INTERVAL,
	"TEMP":       TEMP,
	"TEMPORARY":  TEMPORARY,
	"RENAME":     RENAME,
	"CONSTRAINT": CONSTRAINT,
	// New tokens for PostgreSQL compatibility extensions
	"RECURSIVE":    RECURSIVE,
	"EXPLAIN":      EXPLAIN,
	"VERBOSE":      VERBOSE,
	"FILTER":       FILTER,
	"WITHIN":       WITHIN,
	"MATERIALIZED": MATERIALIZED,
	"REFRESH":      REFRESH,
	"CONCURRENTLY": CONCURRENTLY,
	"ROLLUP":       ROLLUP,
	"CUBE":         CUBE,
	"GROUPING":     GROUPING,
	"SETS":         SETS,
	"FETCH":        FETCH,
	"FIRST":        FIRST,
	"NEXT":         NEXT,
	"ROWS":         ROWS,
	"ROW":          ROW,
	"ONLY":         ONLY,
	"SIMILAR":      SIMILAR,
	"DEFERRABLE":   DEFERRABLE,
	"INITIALLY":    INITIALLY,
	"DEFERRED":     DEFERRED,
	"IMMEDIATE":    IMMEDIATE,
	"GENERATED":    GENERATED,
	"ALWAYS":       ALWAYS,
	"IDENTITY":     IDENTITY,
	"STORED":       STORED,
	"ACTION":       ACTION,
	"CASCADE":      CASCADE,
	"RESTRICT":     RESTRICT,
	// Row-Level Security DDL keywords.
	"POLICY":      POLICY,
	"PERMISSIVE":  PERMISSIVE,
	"RESTRICTIVE": RESTRICTIVE,
	"FORCE":       FORCE,
	"LEVEL":       LEVEL,
	"SECURITY":    SECURITY,
	"ENABLE":      ENABLE,
	"DISABLE":     DISABLE,
	// SERIALIZABLE is rewritten to READ COMMITTED in normalizeSQLForParsing
	// before the lexer sees it, so it intentionally remains an IDENT.
}

// Lex implements the goyacc yyLexer interface. It scans the next token, fills
// lval, and returns the token id (0 on EOF).
func (l *lexer) Lex(lval *yySymType) int {
	l.skipSpaceAndComments()
	if l.pos >= len(l.src) {
		return 0
	}
	c := l.src[l.pos]

	switch {
	case c == '\'':
		return l.scanString(lval)
	case c == '"':
		return l.scanQuotedIdent(lval)
	case c == '$':
		return l.scanParam(lval)
	case isDigit(c):
		return l.scanNumber(lval)
	case isIdentStart(c):
		return l.scanIdentOrKeyword(lval)
	default:
		return l.scanOperatorOrPunct(lval)
	}
}

func (l *lexer) Error(s string) {
	if l.err == nil {
		l.err = &ParseError{
			Msg: s,
			SQLSTATE: "42601", // syntax_error
			Pos: l.pos,
		}
	}
}

func (l *lexer) skipSpaceAndComments() {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			l.pos++
			continue
		}
		// line comment --
		if c == '-' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '-' {
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
			continue
		}
		// block comment /* */
		if c == '/' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '*' {
			l.pos += 2
			for l.pos+1 < len(l.src) && !(l.src[l.pos] == '*' && l.src[l.pos+1] == '/') {
				l.pos++
			}
			l.pos += 2
			continue
		}
		break
	}
}

func (l *lexer) scanString(lval *yySymType) int {
	l.pos++ // opening quote
	var sb strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '\'' {
			// '' is an escaped single quote
			if l.pos+1 < len(l.src) && l.src[l.pos+1] == '\'' {
				sb.WriteByte('\'')
				l.pos += 2
				continue
			}
			l.pos++ // closing quote
			lval.str = sb.String()
			return SCONST
		}
		sb.WriteByte(c)
		l.pos++
	}
	l.Error("unterminated string literal")
	return 0
}

func (l *lexer) scanQuotedIdent(lval *yySymType) int {
	l.pos++ // opening quote
	var sb strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '"' {
			if l.pos+1 < len(l.src) && l.src[l.pos+1] == '"' {
				sb.WriteByte('"')
				l.pos += 2
				continue
			}
			l.pos++
			lval.str = sb.String() // case-preserved
			return IDENT
		}
		sb.WriteByte(c)
		l.pos++
	}
	l.Error("unterminated quoted identifier")
	return 0
}

func (l *lexer) scanParam(lval *yySymType) int {
	l.pos++ // $
	start := l.pos
	for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
		l.pos++
	}
	if l.pos == start {
		l.Error("invalid parameter reference")
		return 0
	}
	n := 0
	for _, ch := range l.src[start:l.pos] {
		n = n*10 + int(ch-'0')
	}
	if n > l.maxParam {
		l.maxParam = n
	}
	lval.ival = n
	return PARAM
}

func (l *lexer) scanNumber(lval *yySymType) int {
	start := l.pos
	isFloat := false
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if isDigit(c) {
			l.pos++
		} else if c == '.' && !isFloat {
			isFloat = true
			l.pos++
		} else {
			break
		}
	}
	lval.str = l.src[start:l.pos]
	if isFloat {
		return FCONST
	}
	return ICONST
}

func (l *lexer) scanIdentOrKeyword(lval *yySymType) int {
	start := l.pos
	for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
		l.pos++
	}
	word := l.src[start:l.pos]
	if tok, ok := keywords[strings.ToUpper(word)]; ok && tok > 0 {
		// `NOT DEFERRABLE` is collapsed into a single NOT_DEFERRABLE token so the
		// trailing constraint-attribute clause stays LALR(1)-unambiguous against a
		// following `NOT NULL` qualifier. DEFERRABLE is a reserved keyword that
		// never validly follows a boolean NOT, so this lookahead is safe.
		if tok == NOT && l.peekIsDeferrable() {
			return NOT_DEFERRABLE
		}
		lval.str = strings.ToUpper(word)
		return tok
	}
	lval.str = strings.ToLower(word) // unquoted identifiers fold to lower-case (PG)
	return IDENT
}

// peekIsDeferrable reports whether the next lexeme is the DEFERRABLE keyword,
// consuming it only in that case. It is used to collapse `NOT DEFERRABLE` into a
// single NOT_DEFERRABLE token. On any other lookahead the input is left
// untouched (l.pos restored), so a following `NULL` etc. is lexed normally.
func (l *lexer) peekIsDeferrable() bool {
	save := l.pos
	l.skipSpaceAndComments()
	if l.pos >= len(l.src) || !isIdentStart(l.src[l.pos]) {
		l.pos = save
		return false
	}
	start := l.pos
	for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
		l.pos++
	}
	if t, found := keywords[strings.ToUpper(l.src[start:l.pos])]; found && t == DEFERRABLE {
		return true // DEFERRABLE consumed
	}
	l.pos = save
	return false
}

// scanOperatorOrPunct handles multi-char operators and single-char punctuation,
// emitting precedence-classed operator tokens so that e.g.
// `u.id = s.attributes ->> 'k'` parses as `u.id = (s.attributes ->> 'k')`.
//
//	JSON_OP ->> -> #>> (tightest binary, JSONB extraction)
//	VECTOR_OP <-> <#> <=> (pgvector distance)
//	COMPARE_OP = < > <= >= <> != (comparison)
//	ADD_OP + -
//	MUL_OP / % (STAR handles *)
//	TYPECAST ::
func (l *lexer) scanOperatorOrPunct(lval *yySymType) int {
	rest := l.src[l.pos:]
	multi := []struct {
		s string
		tok int
	}{
		{"->>", JSON_OP}, {"#>>", JSON_OP}, {"#>", JSON_OP}, {"->", JSON_OP},
		{"<=>", VECTOR_OP}, {"<->", VECTOR_OP}, {"<#>", VECTOR_OP},
		{"<=", COMPARE_OP}, {">=", COMPARE_OP}, {"<>", COMPARE_OP}, {"!=", COMPARE_OP},
		{"&&", CONTAIN_OP},
		{"::", TYPECAST},
	}
	for _, m := range multi {
		if strings.HasPrefix(rest, m.s) {
			l.pos += len(m.s)
			if m.tok == TYPECAST {
				return TYPECAST
			}
			lval.str = m.s
			return m.tok
		}
	}
	c := l.src[l.pos]
	l.pos++
	switch c {
	case '(':
		return '('
	case ')':
		return ')'
	case '[':
		return '['
	case ']':
		return ']'
	case ',':
		return ','
	case '.':
		return '.'
	case ';':
		return ';'
	case '*':
		lval.str = "*"
		return STAR
	case '=', '<', '>':
		lval.str = string(c)
		return COMPARE_OP
	case '+', '-':
		lval.str = string(c)
		return ADD_OP
	case '/', '%':
		lval.str = string(c)
		return MUL_OP
	case '^':
		lval.str = "^"
		return POW_OP
	}
	l.Error(fmt.Sprintf("unexpected character %q", c))
	return 0
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool { return c == '_' || unicode.IsLetter(rune(c)) }
func isIdentPart(c byte) bool { return c == '_' || c == '$' || unicode.IsLetter(rune(c)) || isDigit(c) }
