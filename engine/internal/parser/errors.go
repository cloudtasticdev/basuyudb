package parser

import "fmt"

// ParseError is a structured parse failure carrying a PostgreSQL SQLSTATE so the
// wire layer can return a faithful PG error. (the design specs FR-002-010.)
type ParseError struct {
	Msg string
	SQLSTATE string // e.g. "42601" syntax_error, "XX000" internal (recovered panic)
	Pos int // byte offset into the source, best-effort
	Hint string
}

func (e *ParseError) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s (SQLSTATE %s): %s [hint: %s]", "parse error", e.SQLSTATE, e.Msg, e.Hint)
	}
	return fmt.Sprintf("%s (SQLSTATE %s): %s", "parse error", e.SQLSTATE, e.Msg)
}
