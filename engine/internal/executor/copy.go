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

// CopyTargetOIDs returns the type OIDs of the COPY target columns, in order.
// When columns is empty it returns the OIDs of all table columns (the implicit
// full-column COPY target). The wire layer uses these to drive typed binary
// COPY FROM via ParseCopyDataBinary.
func (e *execImpl) CopyTargetOIDs(ctx context.Context, sess *session.Session, table string, columns []string) ([]uint32, error) {
	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer func() {
		if owns {
			_ = e.commitTx(ctx, txn, owns)
		}
	}()
	sch, err := e.loadSchema(ctx, txn, sess, table)
	if err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		oids := make([]uint32, len(sch.Cols))
		for i, cm := range sch.Cols {
			oids[i] = cm.TypeOID
		}
		return oids, nil
	}
	oids := make([]uint32, len(columns))
	for i, name := range columns {
		idx := sch.colIndex(name)
		if idx < 0 {
			return nil, newExecError("42703", "column %q of relation %q does not exist", name, table)
		}
		oids[i] = sch.Cols[idx].TypeOID
	}
	return oids, nil
}

// FormatCopyData renders result rows as COPY wire payload. For format=="binary"
// it returns the complete PGCOPY binary stream (signature + header + rows +
// trailer); for "csv" a CSV payload; otherwise the PG text format. The binary
// per-field encoding is keyed by each column's type OID in res.Columns.
func FormatCopyData(res *Result, format, delimiter string, header bool) []byte {
	switch format {
	case "binary":
		return formatCopyBinary(res)
	case "csv":
		return formatCopyCSV(res, delimiter, header)
	default:
		return formatCopyText(res, delimiter)
	}
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

// ParseCopyData parses a COPY input stream into rows. For CSV with header the
// first record is skipped. For format=="binary" it decodes the PGCOPY binary
// stream; because the binary stream does not self-describe column type OIDs, the
// fields are decoded with text fallback (each field's raw bytes become the cell
// text). Callers that know the destination column OIDs should use
// ParseCopyDataBinary instead for fully typed decoding (the CopyFrom path then
// re-parses the text per the table's declared types, so binary scalars like
// int4/float8/timestamp are best handled via ParseCopyDataBinary with OIDs).
func ParseCopyData(data []byte, format, delimiter string, header bool, ncols int) ([][]Datum, error) {
	switch format {
	case "binary":
		// No OID context here: decode every field as raw bytes (OIDText path).
		oids := make([]uint32, ncols)
		for i := range oids {
			oids[i] = OIDText
		}
		return parseCopyBinary(data, oids)
	case "csv":
		return parseCopyCSV(data, delimiter, header)
	default:
		return parseCopyText(data, delimiter)
	}
}

// ParseCopyDataBinary decodes a PGCOPY binary stream using the destination
// column type OIDs, producing fully typed PG text for each field (int2/int4/
// int8, float4/float8, bool, text/varchar/bpchar/name, bytea, timestamp[tz],
// date, uuid, json, jsonb, numeric). The wire layer should call this for binary
// COPY FROM, passing the target table's column OIDs.
func ParseCopyDataBinary(data []byte, oids []uint32) ([][]Datum, error) {
	return parseCopyBinary(data, oids)
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
