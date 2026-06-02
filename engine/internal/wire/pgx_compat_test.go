package wire

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// These tests connect to the live wire server with pgx — the de-facto Go
// PostgreSQL driver — to prove real client compatibility (the #1 thing a POC
// checks). Simple protocol exercises the 'Q' path; extended protocol exercises
// Parse/Bind/Execute with parameter binding and result-format negotiation, the
// path ORMs (Prisma, Drizzle, GORM, SQLAlchemy) rely on.

func pgxConnect(t *testing.T, addr string, simple bool) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig("postgres://postgres@" + addr + "/defaultdb?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if simple {
		cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	}
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pgx connect failed: %v", err)
	}
	return conn
}

func TestPgxSimpleProtocol(t *testing.T) {
	addr := startTestServer(t)
	ctx := context.Background()
	conn := pgxConnect(t, addr, true)
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "CREATE TABLE t (id int primary key, n int)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := conn.Exec(ctx, "INSERT INTO t (id, n) VALUES (1, 10)"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var n int
	if err := conn.QueryRow(ctx, "SELECT n FROM t WHERE id = 1").Scan(&n); err != nil {
		t.Fatalf("select scan: %v", err)
	}
	if n != 10 {
		t.Fatalf("want n=10, got %d", n)
	}

	// Real transaction through the driver.
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO t (id, n) VALUES (2, 20)"); err != nil {
		t.Fatalf("tx insert: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var cnt int
	if err := conn.QueryRow(ctx, "SELECT COUNT(*) FROM t").Scan(&cnt); err != nil {
		t.Fatalf("count scan: %v", err)
	}
	if cnt != 2 {
		t.Fatalf("want 2 rows after commit, got %d", cnt)
	}
}

func TestPgxExtendedProtocol(t *testing.T) {
	addr := startTestServer(t)
	ctx := context.Background()
	conn := pgxConnect(t, addr, false) // default: extended + statement cache
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "CREATE TABLE t (id int primary key, n int)"); err != nil {
		t.Fatalf("create table (extended): %v", err)
	}
	if _, err := conn.Exec(ctx, "INSERT INTO t (id, n) VALUES ($1, $2)", 1, 10); err != nil {
		t.Fatalf("parameterized insert: %v", err)
	}
	var n int
	if err := conn.QueryRow(ctx, "SELECT n FROM t WHERE id = $1", 1).Scan(&n); err != nil {
		t.Fatalf("parameterized select: %v", err)
	}
	if n != 10 {
		t.Fatalf("want n=10, got %d", n)
	}
}
