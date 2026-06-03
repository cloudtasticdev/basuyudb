package executor

import (
	"strings"
	"testing"
)

// TestSerialAutoIncrement proves SERIAL columns auto-assign a gap-free,
// monotonically increasing primary key when omitted from the INSERT.
func TestSerialAutoIncrement(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE t (id SERIAL PRIMARY KEY, name TEXT)")
	run(t, ex, sess, "INSERT INTO t (name) VALUES ('alice')")
	run(t, ex, sess, "INSERT INTO t (name) VALUES ('bob')")
	run(t, ex, sess, "INSERT INTO t (name) VALUES ('carol')")

	r := run(t, ex, sess, "SELECT id, name FROM t ORDER BY id")
	if len(r.Rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(r.Rows))
	}
	want := []struct{ id, name string }{{"1", "alice"}, {"2", "bob"}, {"3", "carol"}}
	for i, w := range want {
		if r.Rows[i][0].Text != w.id || r.Rows[i][1].Text != w.name {
			t.Fatalf("row %d: want (%s,%s), got (%s,%s)", i, w.id, w.name, r.Rows[i][0].Text, r.Rows[i][1].Text)
		}
	}
}

// TestDefaultKeywordInValues proves the DEFAULT keyword used as a VALUES item
// (emitted by ORMs like Drizzle: VALUES (default, $1, default)) applies the
// column default rather than erroring.
func TestDefaultKeywordInValues(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE t (id SERIAL PRIMARY KEY, name TEXT, active BOOLEAN DEFAULT true)")
	// All-columns form with DEFAULT for serial id and active.
	run(t, ex, sess, "INSERT INTO t (id, name, active) VALUES (DEFAULT, 'a', DEFAULT)")
	run(t, ex, sess, "INSERT INTO t (id, name, active) VALUES (DEFAULT, 'b', false)")

	r := run(t, ex, sess, "SELECT id, name, active FROM t ORDER BY id")
	if len(r.Rows) != 2 || r.Rows[0][0].Text != "1" || r.Rows[1][0].Text != "2" {
		t.Fatalf("serial via DEFAULT keyword: want ids 1,2, got %v", rowsText(r))
	}
	if r.Rows[0][2].Text != "t" {
		t.Fatalf("active DEFAULT true: want 't', got %q", r.Rows[0][2].Text)
	}
	if r.Rows[1][2].Text != "f" {
		t.Fatalf("explicit false overrides: want 'f', got %q", r.Rows[1][2].Text)
	}
}

// TestColumnDefaults proves constant, now(), and gen_random_uuid() defaults are
// materialized for omitted columns.
func TestColumnDefaults(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, `CREATE TABLE t (
		id INT PRIMARY KEY,
		status TEXT DEFAULT 'active',
		count INT DEFAULT 0,
		uid UUID DEFAULT gen_random_uuid(),
		created TIMESTAMP DEFAULT now()
	)`)
	run(t, ex, sess, "INSERT INTO t (id) VALUES (1)")

	r := run(t, ex, sess, "SELECT status, count, uid, created FROM t WHERE id = 1")
	row := r.Rows[0]
	if row[0].Text != "active" {
		t.Fatalf("status default: want 'active', got %q", row[0].Text)
	}
	if row[1].Text != "0" {
		t.Fatalf("count default: want '0', got %q", row[1].Text)
	}
	if len(row[2].Text) != 36 || strings.Count(row[2].Text, "-") != 4 {
		t.Fatalf("uuid default malformed: %q", row[2].Text)
	}
	if !strings.HasPrefix(row[3].Text, "20") {
		t.Fatalf("now() default malformed: %q", row[3].Text)
	}

	// An explicitly provided value overrides the default.
	run(t, ex, sess, "INSERT INTO t (id, status) VALUES (2, 'archived')")
	r2 := run(t, ex, sess, "SELECT status FROM t WHERE id = 2")
	if r2.Rows[0][0].Text != "archived" {
		t.Fatalf("explicit value should override default, got %q", r2.Rows[0][0].Text)
	}
}

// TestInlineUnique proves an inline UNIQUE column constraint rejects duplicates.
func TestInlineUnique(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY, email TEXT UNIQUE)")
	run(t, ex, sess, "INSERT INTO t (id, email) VALUES (1, 'a@x.com')")

	err := execErr(ex, sess, "INSERT INTO t (id, email) VALUES (2, 'a@x.com')")
	if err == nil {
		t.Fatalf("expected unique violation on duplicate email")
	}
	if !strings.Contains(err.Error(), "unique") && !strings.Contains(err.Error(), "23505") {
		t.Fatalf("expected unique-violation error, got %v", err)
	}

	// A distinct email is accepted.
	run(t, ex, sess, "INSERT INTO t (id, email) VALUES (3, 'b@x.com')")
	r := run(t, ex, sess, "SELECT COUNT(*) FROM t")
	if r.Rows[0][0].Text != "2" {
		t.Fatalf("want 2 rows, got %q", r.Rows[0][0].Text)
	}
}
