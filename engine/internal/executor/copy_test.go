package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/parser"
)

// TestCopyRoundTrip exports a table with COPY TO, re-imports the payload with
// COPY FROM into a second table, and checks the data matches — for both text
// and CSV formats, including NULLs.
func TestCopyRoundTrip(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE src (id INT PRIMARY KEY, name TEXT, note TEXT)")
	run(t, ex, sess, "INSERT INTO src (id, name, note) VALUES (1, 'alice', 'hi')")
	run(t, ex, sess, "INSERT INTO src (id, name, note) VALUES (2, 'bob', NULL)")
	run(t, ex, sess, "INSERT INTO src (id, name) VALUES (3, 'carol')")

	for _, format := range []string{"text", "csv"} {
		t.Run(format, func(t *testing.T) {
			ctx := context.Background()
			// Export.
			out, err := ex.CopyTo(ctx, sess, &ast.CopyStmt{Table: "src", Format: format})
			if err != nil {
				t.Fatalf("CopyTo: %v", err)
			}
			payload := FormatCopyData(out, format, "", false)

			// Re-import into a fresh table.
			run(t, ex, sess, "CREATE TABLE dst_"+format+" (id INT PRIMARY KEY, name TEXT, note TEXT)")
			rows, err := ParseCopyData(payload, format, "", false, 3)
			if err != nil {
				t.Fatalf("ParseCopyData: %v", err)
			}
			n, err := ex.CopyFrom(ctx, sess, "dst_"+format, nil, rows)
			if err != nil {
				t.Fatalf("CopyFrom: %v", err)
			}
			if n != 3 {
				t.Fatalf("%s: want 3 rows loaded, got %d", format, n)
			}

			r := run(t, ex, sess, "SELECT id, name, note FROM dst_"+format+" ORDER BY id")
			if len(r.Rows) != 3 {
				t.Fatalf("%s: want 3 rows, got %d", format, len(r.Rows))
			}
			if r.Rows[0][1].Text != "alice" || r.Rows[0][2].Text != "hi" {
				t.Fatalf("%s row0 mismatch: %v", format, r.Rows[0])
			}
			if !r.Rows[1][2].Null {
				t.Fatalf("%s row1 note should be NULL, got %q", format, r.Rows[1][2].Text)
			}
			if r.Rows[2][1].Text != "carol" || !r.Rows[2][2].Null {
				t.Fatalf("%s row2 mismatch: %v", format, r.Rows[2])
			}
		})
	}
}

// TestCopyColumnSubsetAndQuery covers a column subset on COPY FROM and COPY of a
// query result.
func TestCopyColumnSubsetAndQuery(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)
	ctx := context.Background()

	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY, a TEXT, b TEXT)")

	// COPY FROM with an explicit column subset (b defaults to NULL).
	rows := [][]Datum{{{Text: "1"}, {Text: "x"}}, {{Text: "2"}, {Text: "y"}}}
	n, err := ex.CopyFrom(ctx, sess, "t", []string{"id", "a"}, rows)
	if err != nil || n != 2 {
		t.Fatalf("CopyFrom subset: n=%d err=%v", n, err)
	}
	r := run(t, ex, sess, "SELECT a, b FROM t ORDER BY id")
	if r.Rows[0][0].Text != "x" || !r.Rows[0][1].Null {
		t.Fatalf("subset row0: %v", r.Rows[0])
	}

	// COPY (query) TO STDOUT.
	stmt, _ := parseSelect(t, "SELECT a FROM t WHERE id = 1")
	out, err := ex.CopyTo(ctx, sess, &ast.CopyStmt{Query: stmt, Format: "text"})
	if err != nil {
		t.Fatalf("CopyTo query: %v", err)
	}
	payload := strings.TrimSpace(string(FormatCopyData(out, "text", "", false)))
	if payload != "x" {
		t.Fatalf("COPY (query) want 'x', got %q", payload)
	}
}

func parseSelect(t *testing.T, sql string) (*ast.SelectStmt, error) {
	t.Helper()
	stmt, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	return stmt.(*ast.SelectStmt), nil
}
