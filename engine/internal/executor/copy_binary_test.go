package executor

import (
	"testing"
)

// TestCopyBinaryRoundTrip encodes a result with the PGCOPY binary codec and
// decodes it back, asserting the datums survive across the common type OIDs.
func TestCopyBinaryRoundTrip(t *testing.T) {
	cols := []Column{
		{Name: "i2", TypeOID: OIDInt2},
		{Name: "i4", TypeOID: OIDInt4},
		{Name: "i8", TypeOID: OIDInt8},
		{Name: "f4", TypeOID: OIDFloat4},
		{Name: "f8", TypeOID: OIDFloat8},
		{Name: "b", TypeOID: OIDBool},
		{Name: "t", TypeOID: OIDText},
		{Name: "v", TypeOID: OIDVarchar},
		{Name: "by", TypeOID: OIDBytea},
		{Name: "ts", TypeOID: OIDTimestamp},
		{Name: "d", TypeOID: OIDDate},
		{Name: "u", TypeOID: OIDUUID},
		{Name: "j", TypeOID: OIDJSON},
		{Name: "jb", TypeOID: OIDJSONB},
		{Name: "num", TypeOID: OIDNumeric},
	}
	oids := make([]uint32, len(cols))
	for i, c := range cols {
		oids[i] = c.TypeOID
	}

	rows := [][]Datum{
		{
			{Text: "-12345"},
			{Text: "2000000000"},
			{Text: "9000000000000000000"},
			{Text: "1.5"},
			{Text: "3.14159265358979"},
			{Text: "t"},
			{Text: "hello, world"},
			{Text: "varchar val"},
			{Text: `\xdeadbeef`},
			{Text: "2021-03-04 05:06:07.123456"},
			{Text: "2021-03-04"},
			{Text: "11223344-5566-7788-99aa-bbccddeeff00"},
			{Text: `{"a":1}`},
			{Text: `{"b":2}`},
			{Text: "12345.6789"},
		},
		{
			// A row with NULLs interspersed.
			{Null: true},
			{Text: "0"},
			{Null: true},
			{Text: "-0.25"},
			{Null: true},
			{Text: "f"},
			{Null: true},
			{Text: ""},
			{Null: true},
			{Text: "2000-01-01 00:00:00"},
			{Null: true},
			{Text: "00000000-0000-0000-0000-000000000000"},
			{Null: true},
			{Text: `{}`},
			{Text: "-987.6500"},
		},
	}

	res := &Result{Columns: cols, Rows: rows}
	payload := FormatCopyData(res, "binary", "", false)
	if len(payload) < 19 {
		t.Fatalf("binary payload too short: %d bytes", len(payload))
	}

	got, err := ParseCopyDataBinary(payload, oids)
	if err != nil {
		t.Fatalf("ParseCopyDataBinary: %v", err)
	}
	if len(got) != len(rows) {
		t.Fatalf("want %d rows, got %d", len(rows), len(got))
	}

	// Expected text after a binary round-trip (canonicalized forms).
	want := [][]Datum{
		{
			{Text: "-12345"}, {Text: "2000000000"}, {Text: "9000000000000000000"},
			{Text: "1.5"}, {Text: "3.14159265358979"}, {Text: "t"},
			{Text: "hello, world"}, {Text: "varchar val"}, {Text: `\xdeadbeef`},
			{Text: "2021-03-04 05:06:07.123456"}, {Text: "2021-03-04"},
			{Text: "11223344-5566-7788-99aa-bbccddeeff00"},
			{Text: `{"a":1}`}, {Text: `{"b":2}`}, {Text: "12345.6789"},
		},
		{
			{Null: true}, {Text: "0"}, {Null: true},
			{Text: "-0.25"}, {Null: true}, {Text: "f"},
			{Null: true}, {Text: ""}, {Null: true},
			{Text: "2000-01-01 00:00:00"}, {Null: true},
			{Text: "00000000-0000-0000-0000-000000000000"},
			{Null: true}, {Text: "{}"}, {Text: "-987.6500"},
		},
	}

	for r := range want {
		for c := range want[r] {
			gd, wd := got[r][c], want[r][c]
			if gd.Null != wd.Null {
				t.Fatalf("row %d col %d (%s): null mismatch got=%v want=%v", r, c, cols[c].Name, gd.Null, wd.Null)
			}
			if wd.Null {
				continue
			}
			if gd.Text != wd.Text {
				t.Fatalf("row %d col %d (%s): got %q want %q", r, c, cols[c].Name, gd.Text, wd.Text)
			}
		}
	}
}

// TestCopyBinaryNumeric exercises numeric edge cases through the binary codec.
func TestCopyBinaryNumeric(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0", "0"},
		{"1", "1"},
		{"-1", "-1"},
		{"100", "100"},
		{"10000", "10000"},
		{"99999999", "99999999"},
		{"0.5", "0.5"},
		{"0.0001", "0.0001"},
		{"123456789.987654321", "123456789.987654321"},
		{"-0.250", "-0.250"},
		{"1000000", "1000000"},
		{"NaN", "NaN"},
	}
	for _, tc := range cases {
		enc, ok := encodeCopyNumeric(tc.in)
		if !ok {
			t.Fatalf("encodeCopyNumeric(%q) failed", tc.in)
		}
		got, err := decodeCopyNumeric(enc)
		if err != nil {
			t.Fatalf("decodeCopyNumeric(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("numeric round-trip %q: got %q want %q", tc.in, got, tc.want)
		}
	}
}
