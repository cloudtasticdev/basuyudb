package executor

import (
	"testing"
)

func TestInformationSchemaIntrospection(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE users (id INT PRIMARY KEY, email TEXT, age INT)")
	run(t, ex, sess, "CREATE TABLE orders (id INT PRIMARY KEY, total INT)")
	run(t, ex, sess, "CREATE INDEX idx_email ON users (email)") // must NOT appear as a table

	// information_schema.tables lists the two user tables (not the index def).
	r := run(t, ex, sess, "SELECT table_name FROM information_schema.tables ORDER BY table_name")
	if len(r.Rows) != 2 {
		t.Fatalf("information_schema.tables want 2 rows, got %d: %#v", len(r.Rows), r.Rows)
	}
	if r.Rows[0][0].Text != "orders" || r.Rows[1][0].Text != "users" {
		t.Fatalf("table list want [orders users], got %v %v", r.Rows[0][0].Text, r.Rows[1][0].Text)
	}

	// information_schema.columns, filtered by table — the ORM introspection path.
	c := run(t, ex, sess, "SELECT column_name, data_type, is_nullable FROM information_schema.columns WHERE table_name = 'users' ORDER BY ordinal_position")
	if len(c.Rows) != 3 {
		t.Fatalf("users columns want 3, got %d: %#v", len(c.Rows), c.Rows)
	}
	if c.Rows[0][0].Text != "id" || c.Rows[0][1].Text != "integer" || c.Rows[0][2].Text != "NO" {
		t.Fatalf("first column want id/integer/NO, got %#v", c.Rows[0])
	}
	if c.Rows[1][0].Text != "email" || c.Rows[1][1].Text != "text" {
		t.Fatalf("second column want email/text, got %#v", c.Rows[1])
	}

	// pg_catalog.pg_tables also works.
	p := run(t, ex, sess, "SELECT tablename FROM pg_catalog.pg_tables ORDER BY tablename")
	if len(p.Rows) != 2 {
		t.Fatalf("pg_tables want 2, got %d", len(p.Rows))
	}
}
