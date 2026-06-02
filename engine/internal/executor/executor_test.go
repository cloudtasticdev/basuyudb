package executor

import (
	"context"
	"testing"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
	"github.com/cloudtasticdev/basuyudb/engine/internal/parser"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

func testSession(t *testing.T) *session.Session {
	t.Helper()
	s, err := auth.SessionFromClaims(&auth.SessionClaims{
		Sub: "u", Jti: "j", Role: "service",
		NamespaceAccess: []string{"tenant-a"}, NamespaceID: "tenant-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	return session.New(s, 1, nil)
}

func run(t *testing.T, ex Executor, sess *session.Session, sql string, params ...Datum) *Result {
	t.Helper()
	stmt, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	res, err := ex.Execute(context.Background(), stmt, sess, params)
	if err != nil {
		t.Fatalf("execute %q: %v", sql, err)
	}
	return res
}

// TestGate1SelectOne is the Gate-1 acceptance at the executor layer: SELECT 1
// returns a single int4 column with value "1".
func TestGate1SelectOne(t *testing.T) {
	st, err := storage.Open(storage.Options{DataDir: t.TempDir(), ValueLogFileMB: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ex := New(st, transactions.New(st, 1, nil))
	sess := testSession(t)

	res := run(t, ex, sess, "SELECT 1")
	if len(res.Columns) != 1 || res.Columns[0].TypeOID != OIDInt4 {
		t.Fatalf("want one int4 column, got %#v", res.Columns)
	}
	if len(res.Rows) != 1 || res.Rows[0][0].Text != "1" || res.Rows[0][0].Null {
		t.Fatalf("want single row [1], got %#v", res.Rows)
	}
	if res.Command != "SELECT" {
		t.Fatalf("want SELECT command tag, got %q", res.Command)
	}
}

// TestSelectExpressions exercises arithmetic, comparison, NULL, params, version().
func TestSelectExpressions(t *testing.T) {
	st, _ := storage.Open(storage.Options{DataDir: t.TempDir(), ValueLogFileMB: 4})
	defer st.Close()
	ex := New(st, transactions.New(st, 1, nil))
	sess := testSession(t)

	res := run(t, ex, sess, "SELECT 1 + 2 * 3, 10 / 2, 'hi', 2 > 1, NULL")
	cells := res.Rows[0]
	if cells[0].Text != "7" {
		t.Fatalf("1+2*3 want 7, got %q", cells[0].Text)
	}
	if cells[1].Text != "5" {
		t.Fatalf("10/2 want 5, got %q", cells[1].Text)
	}
	if cells[2].Text != "hi" || cells[2].Null {
		t.Fatalf("string want hi, got %#v", cells[2])
	}
	if cells[3].Text != "t" {
		t.Fatalf("2>1 want t, got %q", cells[3].Text)
	}
	if !cells[4].Null {
		t.Fatalf("NULL want null, got %#v", cells[4])
	}

	// Bound parameter $1.
	res2 := run(t, ex, sess, "SELECT $1", Datum{Text: "hello"})
	if res2.Rows[0][0].Text != "hello" {
		t.Fatalf("param want hello, got %q", res2.Rows[0][0].Text)
	}

	// version() function.
	res3 := run(t, ex, sess, "SELECT version()")
	if res3.Columns[0].Name != "version" {
		t.Fatalf("want column name version, got %q", res3.Columns[0].Name)
	}
}

// TestRelationalCRUD exercises CREATE TABLE, INSERT, SELECT with WHERE,
// UPDATE, and DELETE end-to-end against the managed store.
func TestRelationalCRUD(t *testing.T) {
	st, _ := storage.Open(storage.Options{DataDir: t.TempDir(), ValueLogFileMB: 4})
	defer st.Close()
	ex := New(st, transactions.New(st, 1, nil))
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE users (id text PRIMARY KEY, email text NOT NULL, age int)")
	run(t, ex, sess, "INSERT INTO users (id, email, age) VALUES ('u1', 'a@x.com', '30')")
	run(t, ex, sess, "INSERT INTO users (id, email, age) VALUES ('u2', 'b@x.com', '25')")

	// SELECT * returns both rows.
	all := run(t, ex, sess, "SELECT * FROM users")
	if len(all.Rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(all.Rows))
	}

	// WHERE filter on a column.
	one := run(t, ex, sess, "SELECT email FROM users WHERE id = 'u1'")
	if len(one.Rows) != 1 || one.Rows[0][0].Text != "a@x.com" {
		t.Fatalf("want a@x.com, got %#v", one.Rows)
	}

	// Numeric comparison in WHERE.
	young := run(t, ex, sess, "SELECT id FROM users WHERE age < 28")
	if len(young.Rows) != 1 || young.Rows[0][0].Text != "u2" {
		t.Fatalf("want u2 (age<28), got %#v", young.Rows)
	}

	// UPDATE.
	upd := run(t, ex, sess, "UPDATE users SET email = 'new@x.com' WHERE id = 'u1'")
	if upd.RowsAffected != 1 {
		t.Fatalf("want 1 updated, got %d", upd.RowsAffected)
	}
	check := run(t, ex, sess, "SELECT email FROM users WHERE id = 'u1'")
	if check.Rows[0][0].Text != "new@x.com" {
		t.Fatalf("update not visible, got %q", check.Rows[0][0].Text)
	}

	// Duplicate PK rejected.
	stmt, _ := parser.Parse("INSERT INTO users (id, email) VALUES ('u1', 'dup@x.com')")
	if _, err := ex.Execute(context.Background(), stmt, sess, nil); err == nil {
		t.Fatal("expected duplicate-key error")
	}

	// DELETE.
	del := run(t, ex, sess, "DELETE FROM users WHERE id = 'u2'")
	if del.RowsAffected != 1 {
		t.Fatalf("want 1 deleted, got %d", del.RowsAffected)
	}
	remaining := run(t, ex, sess, "SELECT * FROM users")
	if len(remaining.Rows) != 1 {
		t.Fatalf("want 1 remaining, got %d", len(remaining.Rows))
	}
}

// TestNotNullAndMissingTable verifies constraint + missing-relation errors.
func TestNotNullAndMissingTable(t *testing.T) {
	st, _ := storage.Open(storage.Options{DataDir: t.TempDir(), ValueLogFileMB: 4})
	defer st.Close()
	ex := New(st, transactions.New(st, 1, nil))
	sess := testSession(t)

	stmt, _ := parser.Parse("SELECT * FROM nope")
	if _, err := ex.Execute(context.Background(), stmt, sess, nil); err == nil {
		t.Fatal("expected missing-relation error")
	}

	run(t, ex, sess, "CREATE TABLE t (id text PRIMARY KEY, name text NOT NULL)")
	stmt2, _ := parser.Parse("INSERT INTO t (id) VALUES ('x')")
	_, err := ex.Execute(context.Background(), stmt2, sess, nil)
	ee, ok := err.(*ExecError)
	if !ok || ee.SQLSTATE != "23502" {
		t.Fatalf("want NOT NULL violation 23502, got %v", err)
	}
}
