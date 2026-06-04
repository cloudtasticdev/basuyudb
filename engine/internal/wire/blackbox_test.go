package wire

import (
	"context"
	"testing"
)

// TestBlackBoxDriverHandshake probes the statements real Postgres drivers/ORMs
// send on connect and during everyday use. Each is run through pgx; a failure
// here is a concrete "existing app won't just work" gap. The test reports every
// failure (does not stop at the first) so the whole gap surface is visible.
func TestBlackBoxDriverHandshake(t *testing.T) {
	addr := startTestServer(t)
	ctx := context.Background()
	conn := pgxConnect(t, addr, false)
	defer conn.Close(ctx)

	// (label, sql) pairs that must not error.
	exec := []struct{ label, sql string }{
		{"set search_path", "SET search_path TO public"},
		{"set timezone", "SET TIME ZONE 'UTC'"},
		{"set app name", "SET application_name = 'myapp'"},
		{"set extra_float_digits", "SET extra_float_digits = 3"},
		{"set statement_timeout", "SET statement_timeout = 0"},
		{"set client_encoding", "SET client_encoding = 'UTF8'"},
		{"set session characteristics", "SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL READ COMMITTED"},
		{"begin isolation", "BEGIN ISOLATION LEVEL READ COMMITTED"},
		{"commit", "COMMIT"},
		{"begin read only", "BEGIN READ ONLY"},
		{"rollback", "ROLLBACK"},
		{"deallocate all", "DEALLOCATE ALL"},
		{"discard all", "DISCARD ALL"},
	}
	for _, c := range exec {
		if _, err := conn.Exec(ctx, c.sql); err != nil {
			t.Errorf("EXEC GAP [%s] %q: %v", c.label, c.sql, err)
		}
	}

	// Queries drivers run to learn about the server / introspect — must return a row.
	query := []struct{ label, sql string }{
		{"show transaction_isolation", "SHOW transaction_isolation"},
		{"show standard_conforming_strings", "SHOW standard_conforming_strings"},
		{"show server_version", "SHOW server_version"},
		{"show server_version_num", "SHOW server_version_num"},
		{"show timezone", "SHOW TIME ZONE"},
		{"current_setting", "SELECT current_setting('server_version_num')"},
		{"version()", "SELECT version()"},
		{"current_database", "SELECT current_database()"},
		{"current_schema", "SELECT current_schema()"},
		{"select pg_backend_pid", "SELECT pg_backend_pid()"},
	}
	for _, c := range query {
		var v string
		if err := conn.QueryRow(ctx, c.sql).Scan(&v); err != nil {
			t.Errorf("QUERY GAP [%s] %q: %v", c.label, c.sql, err)
		}
	}
}

// TestBlackBoxCatalogIntrospection exercises the pg_catalog tables and the joins
// across them that ORMs run for migrations / schema introspection (Prisma,
// TypeORM, Rails, drizzle-kit). Each must execute and return rows.
func TestBlackBoxCatalogIntrospection(t *testing.T) {
	addr := startTestServer(t)
	ctx := context.Background()
	conn := pgxConnect(t, addr, false)
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "CREATE TABLE t (id SERIAL PRIMARY KEY, email TEXT UNIQUE, n INT)"); err != nil {
		t.Fatalf("create: %v", err)
	}

	type want struct {
		sql    string
		minRow int
	}
	cases := []want{
		{"SELECT relname FROM pg_class WHERE relkind = 'r'", 1},
		{"SELECT nspname FROM pg_namespace", 1},
		{"SELECT typname FROM pg_type WHERE typname = 'int4'", 1},
		{"SELECT attname FROM pg_attribute WHERE attnum > 0", 1},
		{"SELECT conname FROM pg_constraint WHERE contype = 'p'", 1},
		{"SELECT name, setting FROM pg_settings WHERE name = 'server_version'", 1},
		{"SELECT n.nspname, c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind = 'r'", 1},
		{"SELECT c.relname, a.attname FROM pg_class c JOIN pg_attribute a ON a.attrelid = c.oid WHERE c.relname = 't' AND a.attnum > 0", 1},
		{"SELECT a.attname, t.typname FROM pg_attribute a JOIN pg_type t ON t.oid = a.atttypid WHERE a.attnum > 0", 1},
	}
	for _, c := range cases {
		rows, err := conn.Query(ctx, c.sql)
		if err != nil {
			t.Errorf("CATALOG GAP %q: %v", c.sql, err)
			continue
		}
		n := 0
		for rows.Next() {
			n++
		}
		if e := rows.Err(); e != nil {
			t.Errorf("CATALOG GAP (rows) %q: %v", c.sql, e)
		} else if n < c.minRow {
			t.Errorf("CATALOG %q: want >= %d rows, got %d", c.sql, c.minRow, n)
		}
		rows.Close()
	}
}

// TestBlackBoxQueryShapes probes everyday query shapes and the introspection
// queries ORMs run for migrations/codegen. Each must at least execute (return
// rows or zero rows) without error.
func TestBlackBoxQueryShapes(t *testing.T) {
	addr := startTestServer(t)
	ctx := context.Background()
	conn := pgxConnect(t, addr, false)
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "CREATE TABLE t (id INT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatalf("create: %v", err)
	}

	shapes := []string{
		"SELECT current_user",                       // bare niladic keyword
		"SELECT 1::bigint",                          // cast
		"SELECT CAST(1 AS text)",                    // CAST syntax
		"SELECT 'x'::varchar",                       // text cast
		"SELECT EXISTS (SELECT 1 FROM t)",           // EXISTS subquery
		"SELECT a FROM (SELECT 1 AS a) sub",         // subquery in FROM with alias
		"SELECT id AS \"Id\" FROM t",                // quoted output alias
		"SELECT count(*) FROM information_schema.tables",
		"SELECT table_name FROM information_schema.tables WHERE table_schema = 'public'",
		"SELECT column_name, data_type FROM information_schema.columns WHERE table_name = 't'",
		"SELECT tablename FROM pg_catalog.pg_tables WHERE schemaname = 'public'",
		"SELECT 1 WHERE 1 = 1",                      // FROM-less with WHERE
	}
	for _, sql := range shapes {
		rows, err := conn.Query(ctx, sql)
		if err != nil {
			t.Errorf("SHAPE GAP %q: %v", sql, err)
			continue
		}
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			t.Errorf("SHAPE GAP (rows) %q: %v", sql, err)
		}
		rows.Close()
	}
}
