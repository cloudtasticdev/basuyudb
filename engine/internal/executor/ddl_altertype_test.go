package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudtasticdev/basuyudb/engine/internal/parser"
)

// TestAlterColumnTypeRewrite verifies ALTER COLUMN TYPE physically converts the
// stored values: int→text, text→int (valid), with a USING expression, and that
// an invalid cast aborts without corrupting data.
func TestAlterColumnTypeRewrite(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	// --- int -> text ---
	run(t, ex, sess, "CREATE TABLE t1 (id INT PRIMARY KEY, n INT)")
	run(t, ex, sess, "INSERT INTO t1 (id, n) VALUES (1, 100)")
	run(t, ex, sess, "INSERT INTO t1 (id, n) VALUES (2, 200)")
	run(t, ex, sess, "ALTER TABLE t1 ALTER COLUMN n TYPE TEXT")
	r := run(t, ex, sess, "SELECT n FROM t1 ORDER BY id")
	if r.Columns[0].TypeOID != OIDText {
		t.Fatalf("int->text: col OID want text, got %d", r.Columns[0].TypeOID)
	}
	if r.Rows[0][0].Text != "100" || r.Rows[1][0].Text != "200" {
		t.Fatalf("int->text values: %+v", r.Rows)
	}

	// --- text -> int (valid values) ---
	run(t, ex, sess, "CREATE TABLE t2 (id INT PRIMARY KEY, s TEXT)")
	run(t, ex, sess, "INSERT INTO t2 (id, s) VALUES (1, '42')")
	run(t, ex, sess, "INSERT INTO t2 (id, s) VALUES (2, '-7')")
	run(t, ex, sess, "ALTER TABLE t2 ALTER COLUMN s TYPE INT")
	r = run(t, ex, sess, "SELECT s FROM t2 ORDER BY id")
	if r.Columns[0].TypeOID != OIDInt4 {
		t.Fatalf("text->int: col OID want int4, got %d", r.Columns[0].TypeOID)
	}
	if r.Rows[0][0].Text != "42" || r.Rows[1][0].Text != "-7" {
		t.Fatalf("text->int values: %+v", r.Rows)
	}

	// --- text -> int with an invalid value must abort and not corrupt ---
	run(t, ex, sess, "CREATE TABLE t3 (id INT PRIMARY KEY, s TEXT)")
	run(t, ex, sess, "INSERT INTO t3 (id, s) VALUES (1, '42')")
	run(t, ex, sess, "INSERT INTO t3 (id, s) VALUES (2, 'not a number')")
	stmt, perr := parser.Parse("ALTER TABLE t3 ALTER COLUMN s TYPE INT")
	if perr != nil {
		t.Fatal(perr)
	}
	if _, err := ex.Execute(context.Background(), stmt, sess, nil); err == nil {
		t.Fatal("expected ALTER COLUMN TYPE to fail on uncastable value")
	} else {
		var ee *ExecError
		if !errors.As(err, &ee) || ee.SQLSTATE != "22P02" {
			t.Fatalf("want 22P02, got %v", err)
		}
	}
	// Data must be intact (still TEXT, original values).
	r = run(t, ex, sess, "SELECT s FROM t3 ORDER BY id")
	if r.Columns[0].TypeOID != OIDText {
		t.Fatalf("after failed ALTER, col should still be TEXT, got OID %d", r.Columns[0].TypeOID)
	}
	if r.Rows[0][0].Text != "42" || r.Rows[1][0].Text != "not a number" {
		t.Fatalf("after failed ALTER, values changed: %+v", r.Rows)
	}

	// --- USING expression ---
	run(t, ex, sess, "CREATE TABLE t4 (id INT PRIMARY KEY, n INT)")
	run(t, ex, sess, "INSERT INTO t4 (id, n) VALUES (1, 5)")
	run(t, ex, sess, "INSERT INTO t4 (id, n) VALUES (2, 9)")
	run(t, ex, sess, "ALTER TABLE t4 ALTER COLUMN n TYPE INT USING n * 10")
	r = run(t, ex, sess, "SELECT n FROM t4 ORDER BY id")
	if r.Rows[0][0].Text != "50" || r.Rows[1][0].Text != "90" {
		t.Fatalf("USING n*10 values: %+v", r.Rows)
	}
}

// TestAlterColumnTypeReindex verifies that an index on the altered column still
// serves lookups against the converted values after the rewrite.
func TestAlterColumnTypeReindex(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY, code INT)")
	run(t, ex, sess, "INSERT INTO t (id, code) VALUES (1, 100)")
	run(t, ex, sess, "INSERT INTO t (id, code) VALUES (2, 200)")
	run(t, ex, sess, "CREATE INDEX idx_code ON t (code)")
	run(t, ex, sess, "ALTER TABLE t ALTER COLUMN code TYPE TEXT")

	// Lookup on the converted column returns the right row via the refreshed index.
	r := run(t, ex, sess, "SELECT id FROM t WHERE code = '200'")
	if len(r.Rows) != 1 || r.Rows[0][0].Text != "2" {
		t.Fatalf("post-ALTER index lookup want id 2, got %+v", r.Rows)
	}
}
