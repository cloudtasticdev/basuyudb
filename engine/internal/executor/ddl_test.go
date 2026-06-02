package executor

import (
	"context"
	"fmt"
	"testing"

	"github.com/cloudtasticdev/basuyudb/engine/internal/parser"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
)

// execErr parses and executes sql, returning the error (or nil on success).
func execErr(ex Executor, sess *session.Session, sql string) error {
	stmt, err := parser.Parse(sql)
	if err != nil {
		return err
	}
	_, err = ex.Execute(context.Background(), stmt, sess, nil)
	return err
}

func TestDropTable(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY, n INT)")
	run(t, ex, sess, "INSERT INTO t (id, n) VALUES (1, 10)")
	run(t, ex, sess, "CREATE INDEX idx_n ON t (n)")
	run(t, ex, sess, "DROP TABLE t")

	// Table is gone: selecting it errors.
	if err := execErr(ex, sess, "SELECT * FROM t"); err == nil {
		t.Fatal("expected error selecting dropped table")
	}
	// Recreating works (schema fully removed), and it's empty.
	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY, n INT)")
	if got := len(run(t, ex, sess, "SELECT * FROM t").Rows); got != 0 {
		t.Fatalf("recreated table should be empty, got %d rows", got)
	}
	// DROP TABLE IF EXISTS on a missing table is a no-op.
	run(t, ex, sess, "DROP TABLE IF EXISTS does_not_exist")
}

func TestTruncate(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY, n INT)")
	for i := 1; i <= 5; i++ {
		run(t, ex, sess, fmt.Sprintf("INSERT INTO t (id, n) VALUES (%d, %d)", i, i*10))
	}
	run(t, ex, sess, "CREATE INDEX idx_n ON t (n)")
	run(t, ex, sess, "TRUNCATE TABLE t")

	if got := len(run(t, ex, sess, "SELECT * FROM t").Rows); got != 0 {
		t.Fatalf("truncated table should be empty, got %d", got)
	}
	// Index survives truncate and works after re-insert.
	run(t, ex, sess, "INSERT INTO t (id, n) VALUES (1, 99)")
	if got := len(run(t, ex, sess, "SELECT id FROM t WHERE n = 99").Rows); got != 1 {
		t.Fatalf("index lookup after truncate+insert want 1, got %d", got)
	}
}

func TestAlterTableAddDropColumn(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY, n INT)")
	run(t, ex, sess, "INSERT INTO t (id, n) VALUES (1, 10)")

	// ADD COLUMN: existing row reads new column as NULL.
	run(t, ex, sess, "ALTER TABLE t ADD COLUMN label TEXT")
	r := run(t, ex, sess, "SELECT id, n, label FROM t")
	if len(r.Rows) != 1 || !r.Rows[0][2].Null {
		t.Fatalf("existing row should read new column as NULL, got %#v", r.Rows)
	}
	// New inserts populate the new column.
	run(t, ex, sess, "INSERT INTO t (id, n, label) VALUES (2, 20, 'hi')")
	r2 := run(t, ex, sess, "SELECT label FROM t WHERE id = 2")
	if r2.Rows[0][0].Text != "hi" {
		t.Fatalf("new column value want 'hi', got %q", r2.Rows[0][0].Text)
	}

	// DROP COLUMN rewrites rows; remaining columns intact.
	run(t, ex, sess, "ALTER TABLE t DROP COLUMN n")
	r3 := run(t, ex, sess, "SELECT id, label FROM t WHERE id = 2")
	if r3.Rows[0][0].Text != "2" || r3.Rows[0][1].Text != "hi" {
		t.Fatalf("after drop column, want id=2/label=hi, got %#v", r3.Rows)
	}
	// Referencing the dropped column errors.
	if err := execErr(ex, sess, "SELECT n FROM t"); err == nil {
		t.Fatal("expected error referencing dropped column")
	}
	// Cannot drop the PK column.
	if err := execErr(ex, sess, "ALTER TABLE t DROP COLUMN id"); err == nil {
		t.Fatal("expected error dropping PK column")
	}
}
