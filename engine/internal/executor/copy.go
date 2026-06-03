package executor

import (
	"bytes"
	"context"
	"encoding/csv"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
)

// CopyTo produces the rows for COPY ... TO STDOUT — either the embedded query or
// a scan of the named table (optionally a subset of columns).
func (e *execImpl) CopyTo(ctx context.Context, sess *session.Session, c *ast.CopyStmt) (*Result, error) {
	if c.Query != nil {
		sel, ok := c.Query.(*ast.SelectStmt)
		if !ok {
			return nil, newExecError("0A000", "COPY (query) must be a SELECT")
		}
		return e.execSelect(ctx, sel, sess, nil)
	}
	var targets []*ast.ResTarget
	if len(c.Columns) > 0 {
		for _, col := range c.Columns {
			targets = append(targets, &ast.ResTarget{Val: &ast.ColumnRef{Fields: []string{col}}})
		}
	} else {
		targets = []*ast.ResTarget{{Val: &ast.A_Star{}}}
	}
	sel := &ast.SelectStmt{TargetList: targets, FromClause: []ast.Node{&ast.RangeVar{RelName: c.Table}}}
	return e.execSelect(ctx, sel, sess, nil)
}

// CopyFrom bulk-loads parsed rows into a table, reusing the INSERT path per row
// so defaults, sequences, and constraints all apply.
func (e *execImpl) CopyFrom(ctx context.Context, sess *session.Session, table string, columns []string, rows [][]Datum) (int64, error) {
	var cols []*ast.ResTarget
	for _, c := range columns {
		cols = append(cols, &ast.ResTarget{Name: c})
	}
	var n int64
	for _, row := range rows {
		vals := make([]*ast.ResTarget, len(row))
		for i, d := range row {
			if d.Null {
				vals[i] = &ast.ResTarget{Val: &ast.A_Const{Type: ast.ConstNull}}
			} else {
				vals[i] = &ast.ResTarget{Val: &ast.A_Const{Type: ast.ConstString, Val: d.Text}}
			}
		}
		ins := &ast.InsertStmt{
			Relation:   &ast.RangeVar{RelName: table},
			Cols:       cols,
			SelectStmt: &ast.SelectStmt{TargetList: vals},
		}
		if _, err := e.Execute(ctx, ins, sess, nil); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// FormatCopyData renders result rows as COPY wire payload (text or CSV).
func FormatCopyData(res *Result, format, delimiter string, header bool) []byte {
	if format == "csv" {
		return formatCopyCSV(res, delimiter, header)
	}
	return formatCopyText(res, delimiter)
}

func formatCopyText(res *Result, delimiter string) []byte {
	delim := "\t"
	if delimiter != "" {
		delim = delimiter
	}
	var b bytes.Buffer
	for _, row := range res.Rows {
		for i, d := range row {
			if i > 0 {
				b.WriteString(delim)
			}
			if d.Null {
				b.WriteString("\\N")
			} else {
				b.WriteString(escapeCopyText(d.Text))
			}
		}
		b.WriteByte('\n')
	}
	return b.Bytes()
}

func escapeCopyText(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "\t", "\\t", "\n", "\\n", "\r", "\\r")
	return r.Replace(s)
}

func formatCopyCSV(res *Result, delimiter string, header bool) []byte {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	if delimiter != "" {
		w.Comma = rune(delimiter[0])
	}
	if header {
		hrow := make([]string, len(res.Columns))
		for i, c := range res.Columns {
			hrow[i] = c.Name
		}
		_ = w.Write(hrow)
	}
	for _, row := range res.Rows {
		rec := make([]string, len(row))
		for i, d := range row {
			if d.Null {
				rec[i] = "" // CSV NULL is the empty unquoted field
			} else {
				rec[i] = d.Text
			}
		}
		_ = w.Write(rec)
	}
	w.Flush()
	return b.Bytes()
}

// ParseCopyData parses a COPY input stream into rows (text or CSV). For CSV with
// header, the first record is skipped.
func ParseCopyData(data []byte, format, delimiter string, header bool, ncols int) ([][]Datum, error) {
	if format == "csv" {
		return parseCopyCSV(data, delimiter, header)
	}
	return parseCopyText(data, delimiter)
}

func parseCopyText(data []byte, delimiter string) ([][]Datum, error) {
	delim := "\t"
	if delimiter != "" {
		delim = delimiter
	}
	var rows [][]Datum
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		if line == "\\." { // end-of-data marker
			break
		}
		fields := strings.Split(line, delim)
		row := make([]Datum, len(fields))
		for i, f := range fields {
			if f == "\\N" {
				row[i] = Datum{Null: true}
			} else {
				row[i] = Datum{Text: unescapeCopyText(f)}
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func unescapeCopyText(s string) string {
	r := strings.NewReplacer("\\t", "\t", "\\n", "\n", "\\r", "\r", "\\\\", "\\")
	return r.Replace(s)
}

func parseCopyCSV(data []byte, delimiter string, header bool) ([][]Datum, error) {
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	if delimiter != "" {
		r.Comma = rune(delimiter[0])
	}
	recs, err := r.ReadAll()
	if err != nil {
		return nil, newExecError("22P04", "invalid CSV in COPY: %v", err)
	}
	var rows [][]Datum
	for i, rec := range recs {
		if header && i == 0 {
			continue
		}
		row := make([]Datum, len(rec))
		for j, f := range rec {
			if f == "" {
				row[j] = Datum{Null: true}
			} else {
				row[j] = Datum{Text: f}
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}
