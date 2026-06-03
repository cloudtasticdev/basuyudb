package executor

import (
	"testing"
)

func TestRealPostgresTypes(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	// A schema using the full Postgres type vocabulary that previously failed
	// to even parse (VARCHAR(n), NUMERIC(p,s), TIMESTAMP, UUID, JSONB, ...).
	run(t, ex, sess, `CREATE TABLE t (
		id UUID PRIMARY KEY,
		name VARCHAR(255),
		amount NUMERIC(10,2),
		qty SMALLINT,
		ratio DOUBLE PRECISION,
		created TIMESTAMP,
		updated TIMESTAMP WITH TIME ZONE,
		birthday DATE,
		meta JSONB,
		active BOOLEAN
	)`)

	// information_schema reports the correct data types for the ORM.
	r := run(t, ex, sess, "SELECT column_name, data_type FROM information_schema.columns WHERE table_name = 't' ORDER BY ordinal_position")
	want := map[string]string{
		"id": "uuid", "name": "character varying", "amount": "numeric",
		"qty": "smallint", "ratio": "double precision",
		"created": "timestamp without time zone", "updated": "timestamp with time zone",
		"birthday": "date", "meta": "jsonb", "active": "boolean",
	}
	for _, row := range r.Rows {
		col, dt := row[0].Text, row[1].Text
		if want[col] != dt {
			t.Fatalf("column %q: want data_type %q, got %q", col, want[col], dt)
		}
	}

	// Insert + read back a row with these types.
	run(t, ex, sess, `INSERT INTO t (id, name, amount, qty, created, active)
		VALUES ('550e8400-e29b-41d4-a716-446655440000', 'widget', '19.99', 5, '2026-01-15 10:30:00', true)`)
	got := run(t, ex, sess, "SELECT name, amount, created FROM t")
	if got.Rows[0][0].Text != "widget" || got.Rows[0][1].Text != "19.99" {
		t.Fatalf("typed row round-trip failed: %#v", got.Rows[0])
	}
	// Result columns carry the real OIDs.
	if got.Columns[1].TypeOID != OIDNumeric {
		t.Fatalf("amount column OID want numeric(%d), got %d", OIDNumeric, got.Columns[1].TypeOID)
	}
}

func TestCasts(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)
	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY)")
	run(t, ex, sess, "INSERT INTO t (id) VALUES (1)")

	r := run(t, ex, sess, "SELECT '42'::int, '3.14'::float8, 'hi'::varchar")
	if r.Columns[0].TypeOID != OIDInt4 || r.Columns[1].TypeOID != OIDFloat8 || r.Columns[2].TypeOID != OIDVarchar {
		t.Fatalf("cast OIDs wrong: %#v", r.Columns)
	}
	if r.Rows[0][0].Text != "42" {
		t.Fatalf("cast value wrong: %q", r.Rows[0][0].Text)
	}
}
