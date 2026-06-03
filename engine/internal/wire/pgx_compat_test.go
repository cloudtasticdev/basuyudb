package wire

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestPgxCopy exercises the COPY sub-protocol end-to-end through pgx's low-level
// pgconn (text format): COPY FROM STDIN to bulk-load, then COPY TO STDOUT to
// export — the path psql \copy and bulk loaders use.
func TestPgxCopy(t *testing.T) {
	addr := startTestServer(t)
	ctx := context.Background()
	conn := pgxConnect(t, addr, true) // simple protocol
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "CREATE TABLE t (id INT PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatalf("create: %v", err)
	}

	// COPY FROM STDIN (text): two rows.
	in := strings.NewReader("1\talice\n2\tbob\n")
	tag, err := conn.PgConn().CopyFrom(ctx, in, "COPY t (id, name) FROM STDIN")
	if err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if tag.RowsAffected() != 2 {
		t.Fatalf("CopyFrom rows: want 2, got %d", tag.RowsAffected())
	}

	var n int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM t").Scan(&n); err != nil || n != 2 {
		t.Fatalf("count after copy: n=%d err=%v", n, err)
	}

	// COPY TO STDOUT (text): read it back.
	var buf bytes.Buffer
	if _, err := conn.PgConn().CopyTo(ctx, &buf, "COPY t TO STDOUT"); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "alice") || !strings.Contains(got, "bob") {
		t.Fatalf("CopyTo output missing rows: %q", got)
	}
}

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

// TestPgxTypedColumns proves the binary result encoders for the richer types
// (smallint, timestamp) round-trip through pgx's extended protocol, which
// requests binary for these OIDs.
func TestPgxTypedColumns(t *testing.T) {
	addr := startTestServer(t)
	ctx := context.Background()
	conn := pgxConnect(t, addr, false) // extended protocol
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "CREATE TABLE t (id INT PRIMARY KEY, qty SMALLINT, created TIMESTAMP)"); err != nil {
		t.Fatalf("create typed table: %v", err)
	}
	if _, err := conn.Exec(ctx, "INSERT INTO t (id, qty, created) VALUES (1, 7, '2026-01-15 10:30:00')"); err != nil {
		t.Fatalf("insert typed: %v", err)
	}

	var qty int16
	var created time.Time
	if err := conn.QueryRow(ctx, "SELECT qty, created FROM t WHERE id = 1").Scan(&qty, &created); err != nil {
		t.Fatalf("typed scan (binary): %v", err)
	}
	if qty != 7 {
		t.Fatalf("smallint binary want 7, got %d", qty)
	}
	if created.Year() != 2026 || created.Month() != 1 || created.Day() != 15 || created.Hour() != 10 {
		t.Fatalf("timestamp binary mismatch: %v", created)
	}
}

// TestPgxSerialReturning mirrors the canonical ORM insert: a SERIAL primary key
// auto-assigned by the database and read back via RETURNING — the pattern every
// ORM (Prisma, Drizzle, GORM, ActiveRecord) uses to obtain the new row's id.
func TestPgxSerialReturning(t *testing.T) {
	addr := startTestServer(t)
	ctx := context.Background()
	conn := pgxConnect(t, addr, false) // extended protocol
	defer conn.Close(ctx)

	if _, err := conn.Exec(ctx, "CREATE TABLE users (id SERIAL PRIMARY KEY, email TEXT UNIQUE NOT NULL, created TIMESTAMP DEFAULT now())"); err != nil {
		t.Fatalf("create users: %v", err)
	}

	var id1, id2 int
	if err := conn.QueryRow(ctx, "INSERT INTO users (email) VALUES ($1) RETURNING id", "a@x.com").Scan(&id1); err != nil {
		t.Fatalf("insert returning id (1): %v", err)
	}
	if err := conn.QueryRow(ctx, "INSERT INTO users (email) VALUES ($1) RETURNING id", "b@x.com").Scan(&id2); err != nil {
		t.Fatalf("insert returning id (2): %v", err)
	}
	if id1 != 1 || id2 != 2 {
		t.Fatalf("serial RETURNING want ids 1,2 got %d,%d", id1, id2)
	}

	// The UNIQUE constraint rejects a duplicate email through the driver.
	if _, err := conn.Exec(ctx, "INSERT INTO users (email) VALUES ($1)", "a@x.com"); err == nil {
		t.Fatalf("expected unique violation on duplicate email")
	}
}
