package executor

import "testing"

// TestPowerAndMod covers the ^ exponentiation operator and the mod() function.
func TestPowerAndMod(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	cases := []struct{ sql, want string }{
		{"SELECT 2 ^ 10", "1024"},
		{"SELECT 2.0 ^ 3", "8"},
		{"SELECT mod(9, 4)", "1"},
		{"SELECT mod(10, 3)", "1"},
		{"SELECT mod(17, 5)", "2"},
	}
	for _, c := range cases {
		r := run(t, ex, sess, c.sql)
		if r.Rows[0][0].Text != c.want {
			t.Fatalf("%s: want %q, got %q", c.sql, c.want, r.Rows[0][0].Text)
		}
	}
}

// TestArrayOverlapAndPosition covers the && overlap operator and array_position.
func TestArrayOverlapAndPosition(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	r := run(t, ex, sess, "SELECT ARRAY[1,2,3] && ARRAY[3,4,5]")
	if r.Rows[0][0].Text != "t" {
		t.Fatalf("overlap (shared): want t, got %q", r.Rows[0][0].Text)
	}
	r = run(t, ex, sess, "SELECT ARRAY[1,2,3] && ARRAY[7,8,9]")
	if r.Rows[0][0].Text != "f" {
		t.Fatalf("overlap (disjoint): want f, got %q", r.Rows[0][0].Text)
	}
	r = run(t, ex, sess, "SELECT array_position(ARRAY['a','b','c'], 'b')")
	if r.Rows[0][0].Text != "2" {
		t.Fatalf("array_position: want 2, got %q", r.Rows[0][0].Text)
	}
	r = run(t, ex, sess, "SELECT array_position(ARRAY['a','b','c'], 'z')")
	if !r.Rows[0][0].Null {
		t.Fatalf("array_position absent: want NULL, got %q", r.Rows[0][0].Text)
	}
}

// TestRowComparison covers ROW(...) lexicographic comparison and row IN-lists.
func TestRowComparison(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	cases := []struct{ sql, want string }{
		{"SELECT (1,2) = (1,2)", "t"},
		{"SELECT (1,2) = (1,3)", "f"},
		{"SELECT (1,2) < (1,3)", "t"},
		{"SELECT (2,0) > (1,9)", "t"},
		{"SELECT (1,2) <= (1,2)", "t"},
		{"SELECT (1,2) <> (3,4)", "t"},
		{"SELECT ROW(1,2) = ROW(1,2)", "t"},
		{"SELECT (1,2) IN ((3,4),(1,2))", "t"},
		{"SELECT (1,2) IN ((3,4),(5,6))", "f"},
	}
	for _, c := range cases {
		r := run(t, ex, sess, c.sql)
		if r.Rows[0][0].Text != c.want {
			t.Fatalf("%s: want %q, got %q", c.sql, c.want, r.Rows[0][0].Text)
		}
	}
}

// TestJSONBOps covers #>, jsonb_set, and the jsonb - delete operator.
func TestJSONBOps(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	// #> extracts a sub-object as json.
	r := run(t, ex, sess, `SELECT '{"a":{"b":42}}'::jsonb #> '{a,b}'`)
	if r.Rows[0][0].Text != "42" {
		t.Fatalf("#> : want 42, got %q", r.Rows[0][0].Text)
	}
	r = run(t, ex, sess, `SELECT '{"a":{"b":{"c":1}}}'::jsonb #> '{a,b}'`)
	if r.Rows[0][0].Text != `{"c":1}` {
		t.Fatalf("#> object: want {\"c\":1}, got %q", r.Rows[0][0].Text)
	}

	// jsonb_set updates an existing key.
	r = run(t, ex, sess, `SELECT jsonb_set('{"a":1,"b":2}'::jsonb, '{a}', '9')`)
	if r.Rows[0][0].Text != `{"a":9,"b":2}` && r.Rows[0][0].Text != `{"b":2,"a":9}` {
		t.Fatalf("jsonb_set update: got %q", r.Rows[0][0].Text)
	}

	// jsonb_set creates a missing key when create_missing defaults true.
	r = run(t, ex, sess, `SELECT jsonb_set('{"a":1}'::jsonb, '{c}', '3')`)
	if r.Rows[0][0].Text != `{"a":1,"c":3}` && r.Rows[0][0].Text != `{"c":3,"a":1}` {
		t.Fatalf("jsonb_set create: got %q", r.Rows[0][0].Text)
	}

	// jsonb - text removes a key.
	r = run(t, ex, sess, `SELECT '{"a":1,"b":2}'::jsonb - 'a'`)
	if r.Rows[0][0].Text != `{"b":2}` {
		t.Fatalf("jsonb - text: want {\"b\":2}, got %q", r.Rows[0][0].Text)
	}

	// jsonb - int removes an array element by index.
	r = run(t, ex, sess, `SELECT '[10,20,30]'::jsonb - 1`)
	if r.Rows[0][0].Text != `[10,30]` {
		t.Fatalf("jsonb - int: want [10,30], got %q", r.Rows[0][0].Text)
	}
}

// TestUnnest covers unnest() in the SELECT list and in the FROM clause.
func TestUnnest(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	// SELECT-list expansion.
	r := run(t, ex, sess, "SELECT unnest(ARRAY[10,20,30])")
	if len(r.Rows) != 3 {
		t.Fatalf("unnest SELECT: want 3 rows, got %d: %#v", len(r.Rows), r.Rows)
	}
	if r.Rows[0][0].Text != "10" || r.Rows[2][0].Text != "30" {
		t.Fatalf("unnest SELECT values: %#v", r.Rows)
	}

	// FROM-clause expansion with alias.
	r = run(t, ex, sess, "SELECT x FROM unnest(ARRAY['a','b','c']) AS u(x)")
	if len(r.Rows) != 3 {
		t.Fatalf("unnest FROM: want 3 rows, got %d", len(r.Rows))
	}
	if r.Rows[1][0].Text != "b" {
		t.Fatalf("unnest FROM value: want b, got %q", r.Rows[1][0].Text)
	}
}

// TestJSONBArrayElements covers jsonb_array_elements in SELECT and FROM.
func TestJSONBArrayElements(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	r := run(t, ex, sess, `SELECT jsonb_array_elements('[1,2,3]'::jsonb)`)
	if len(r.Rows) != 3 {
		t.Fatalf("jsonb_array_elements SELECT: want 3 rows, got %d: %#v", len(r.Rows), r.Rows)
	}
	r = run(t, ex, sess, `SELECT jsonb_array_elements_text('["x","y"]'::jsonb)`)
	if len(r.Rows) != 2 || r.Rows[0][0].Text != "x" || r.Rows[1][0].Text != "y" {
		t.Fatalf("jsonb_array_elements_text: %#v", r.Rows)
	}
}

// TestDeferrableColumnConstraint verifies CREATE TABLE with a DEFERRABLE column
// constraint executes without error (the executor ignores the flag).
func TestDeferrableColumnConstraint(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE t (id int PRIMARY KEY, ref int UNIQUE DEFERRABLE INITIALLY DEFERRED)")
	run(t, ex, sess, "INSERT INTO t (id, ref) VALUES (1, 100)")
	r := run(t, ex, sess, "SELECT id, ref FROM t")
	if len(r.Rows) != 1 || r.Rows[0][0].Text != "1" {
		t.Fatalf("deferrable table: %#v", r.Rows)
	}
}
