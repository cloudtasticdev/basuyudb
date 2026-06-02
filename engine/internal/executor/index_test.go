package executor

import (
	"context"
	"fmt"
	"testing"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/parser"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

func newIdxExec(t *testing.T) (Executor, func()) {
	t.Helper()
	st, err := storage.Open(storage.Options{DataDir: t.TempDir(), ValueLogFileMB: 4})
	if err != nil {
		t.Fatal(err)
	}
	ex := New(st, transactions.New(st, 1, nil))
	return ex, func() { st.Close() }
}

// TestSecondaryIndexLookup verifies that CREATE INDEX, index maintenance across
// INSERT/UPDATE/DELETE, and the index-scan planner all return correct rows.
func TestSecondaryIndexLookup(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE users (id INT PRIMARY KEY, email TEXT, city TEXT)")
	for i := 1; i <= 50; i++ {
		run(t, ex, sess, fmt.Sprintf("INSERT INTO users (id, email, city) VALUES (%d, 'u%d@x.io', 'city%d')", i, i, i%5))
	}

	// Index on a non-PK column. Back-fill must pick up the 50 existing rows.
	run(t, ex, sess, "CREATE INDEX idx_city ON users (city)")

	// 50 rows, city = i%5 → 10 rows per city value.
	res := run(t, ex, sess, "SELECT id FROM users WHERE city = 'city3'")
	if len(res.Rows) != 10 {
		t.Fatalf("city3 want 10 rows, got %d", len(res.Rows))
	}

	// Confirm the index path was actually taken (not a coincidental full scan).
	rows, used, err := ex.(*execImpl).indexScanProbe(t, sess, "SELECT id FROM users WHERE city = 'city3'")
	if err != nil {
		t.Fatal(err)
	}
	if !used {
		t.Fatal("expected index scan path to be used")
	}
	if len(rows) != 10 {
		t.Fatalf("index probe want 10 rows, got %d", len(rows))
	}

	// INSERT maintains the index.
	run(t, ex, sess, "INSERT INTO users (id, email, city) VALUES (100, 'new@x.io', 'city3')")
	if got := len(run(t, ex, sess, "SELECT id FROM users WHERE city = 'city3'").Rows); got != 11 {
		t.Fatalf("after insert want 11, got %d", got)
	}

	// UPDATE moves a row out of city3.
	run(t, ex, sess, "UPDATE users SET city = 'cityX' WHERE id = 100")
	if got := len(run(t, ex, sess, "SELECT id FROM users WHERE city = 'city3'").Rows); got != 10 {
		t.Fatalf("after update want 10, got %d", got)
	}
	if got := len(run(t, ex, sess, "SELECT id FROM users WHERE city = 'cityX'").Rows); got != 1 {
		t.Fatalf("after update want 1 cityX, got %d", got)
	}

	// DELETE removes the index entry.
	run(t, ex, sess, "DELETE FROM users WHERE id = 100")
	if got := len(run(t, ex, sess, "SELECT id FROM users WHERE city = 'cityX'").Rows); got != 0 {
		t.Fatalf("after delete want 0 cityX, got %d", got)
	}
}

// TestUniqueIndexEnforcement verifies UNIQUE index rejects duplicate values on
// both INSERT and UPDATE, and on back-fill.
func TestUniqueIndexEnforcement(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE accounts (id INT PRIMARY KEY, email TEXT)")
	run(t, ex, sess, "INSERT INTO accounts (id, email) VALUES (1, 'a@x.io')")
	run(t, ex, sess, "INSERT INTO accounts (id, email) VALUES (2, 'b@x.io')")
	run(t, ex, sess, "CREATE UNIQUE INDEX uq_email ON accounts (email)")

	// Duplicate INSERT must fail with 23505.
	stmt, _ := parser.Parse("INSERT INTO accounts (id, email) VALUES (3, 'a@x.io')")
	if _, err := ex.Execute(context.Background(), stmt, sess, nil); err == nil {
		t.Fatal("expected unique violation on duplicate insert")
	} else if ee, ok := err.(*ExecError); !ok || ee.SQLSTATE != "23505" {
		t.Fatalf("want 23505, got %v", err)
	}

	// Distinct INSERT still works.
	run(t, ex, sess, "INSERT INTO accounts (id, email) VALUES (3, 'c@x.io')")

	// UPDATE into an existing value must fail.
	stmt, _ = parser.Parse("UPDATE accounts SET email = 'a@x.io' WHERE id = 3")
	if _, err := ex.Execute(context.Background(), stmt, sess, nil); err == nil {
		t.Fatal("expected unique violation on update collision")
	} else if ee, ok := err.(*ExecError); !ok || ee.SQLSTATE != "23505" {
		t.Fatalf("want 23505 on update, got %v", err)
	}

	// UNIQUE back-fill on duplicate data must fail.
	run(t, ex, sess, "CREATE TABLE dups (id INT PRIMARY KEY, k TEXT)")
	run(t, ex, sess, "INSERT INTO dups (id, k) VALUES (1, 'same')")
	run(t, ex, sess, "INSERT INTO dups (id, k) VALUES (2, 'same')")
	stmt, _ = parser.Parse("CREATE UNIQUE INDEX uq_k ON dups (k)")
	if _, err := ex.Execute(context.Background(), stmt, sess, nil); err == nil {
		t.Fatal("expected unique violation creating index over duplicate data")
	}
}

// indexScanProbe is a test-only helper that runs the planner's index fast-path
// directly so a test can assert the index (not a full scan) served the query.
func (e *execImpl) indexScanProbe(t *testing.T, sess *session.Session, sql string) ([]boundRow, bool, error) {
	t.Helper()
	stmt, err := parser.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	sel := stmt.(*ast.SelectStmt)
	txn, err := e.txn.Begin(context.Background(), sess.Auth)
	if err != nil {
		return nil, false, err
	}
	defer e.txn.Rollback(context.Background(), txn)
	scan, err := e.planIndexScan(context.Background(), txn, sess, sel, nil)
	if err != nil {
		return nil, false, err
	}
	if scan == nil {
		return nil, false, nil
	}
	return scan.rows, true, nil
}
