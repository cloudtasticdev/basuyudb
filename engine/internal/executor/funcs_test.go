package executor

import "testing"

// TestCaseExpr covers both the searched and simple CASE forms, including ELSE
// and the no-match NULL fallthrough.
func TestCaseExpr(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	// Searched CASE.
	r := run(t, ex, sess, "SELECT CASE WHEN 1 > 2 THEN 'a' WHEN 3 > 2 THEN 'b' ELSE 'c' END")
	if r.Rows[0][0].Text != "b" {
		t.Fatalf("searched CASE: want 'b', got %q", r.Rows[0][0].Text)
	}
	// ELSE branch.
	r = run(t, ex, sess, "SELECT CASE WHEN false THEN 'a' ELSE 'z' END")
	if r.Rows[0][0].Text != "z" {
		t.Fatalf("CASE ELSE: want 'z', got %q", r.Rows[0][0].Text)
	}
	// No match, no ELSE -> NULL.
	r = run(t, ex, sess, "SELECT CASE WHEN false THEN 'a' END")
	if !r.Rows[0][0].Null {
		t.Fatalf("CASE no-match: want NULL, got %q", r.Rows[0][0].Text)
	}
	// Simple CASE.
	r = run(t, ex, sess, "SELECT CASE 2 WHEN 1 THEN 'one' WHEN 2 THEN 'two' ELSE 'many' END")
	if r.Rows[0][0].Text != "two" {
		t.Fatalf("simple CASE: want 'two', got %q", r.Rows[0][0].Text)
	}
}

// TestScalarFunctions covers COALESCE/NULLIF and the string/math helpers.
func TestScalarFunctions(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	cases := []struct{ sql, want string }{
		{"SELECT COALESCE(NULL, NULL, 'x')", "x"},
		{"SELECT COALESCE(NULL, 'first', 'second')", "first"},
		{"SELECT NULLIF(5, 5)", ""},      // equal -> NULL
		{"SELECT NULLIF(5, 6)", "5"},     // unequal -> left
		{"SELECT UPPER('abc')", "ABC"},
		{"SELECT LOWER('ABC')", "abc"},
		{"SELECT LENGTH('hello')", "5"},
		{"SELECT TRIM('  hi  ')", "hi"},
		{"SELECT CONCAT('a', 'b', 'c')", "abc"},
		{"SELECT ABS(-7)", "7"},
		{"SELECT CEIL(4.2)", "5"},
		{"SELECT FLOOR(4.8)", "4"},
		{"SELECT ROUND(4.5)", "5"},
	}
	for _, c := range cases {
		r := run(t, ex, sess, c.sql)
		got := r.Rows[0][0]
		if c.want == "" {
			if !got.Null {
				t.Fatalf("%s: want NULL, got %q", c.sql, got.Text)
			}
			continue
		}
		if got.Text != c.want {
			t.Fatalf("%s: want %q, got %q", c.sql, c.want, got.Text)
		}
	}
}
