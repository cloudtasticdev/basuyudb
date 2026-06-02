package executor

import (
	"context"
	"testing"

	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// TestPersistenceAcrossRestart proves committed data (schema + rows + branches)
// survives a clean store close and reopen on the same data directory — the
// restart durability the live demo could not isolate from a hard process kill.
func TestPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()

	// --- session 1: write schema, rows, and a branch ---
	st1, err := storage.Open(storage.Options{DataDir: dir, ValueLogFileMB: 4})
	if err != nil {
		t.Fatal(err)
	}
	ex1 := New(st1, transactions.New(st1, 1, nil))
	sess := testSession(t)

	run(t, ex1, sess, "CREATE TABLE accounts (id text PRIMARY KEY, balance int)")
	for _, v := range [][2]string{{"a1", "100"}, {"a2", "250"}, {"a3", "75"}} {
		run(t, ex1, sess, "INSERT INTO accounts (id, balance) VALUES ('"+v[0]+"', '"+v[1]+"')")
	}
	run(t, ex1, sess, "UPDATE accounts SET balance = '999' WHERE id = 'a2'")
	run(t, ex1, sess, "CREATE BRANCH audit FROM main")

	// Clean shutdown (flushes BadgerDB) — what the engine's SIGTERM handler does.
	if err := st1.Close(); err != nil {
		t.Fatalf("close session 1: %v", err)
	}

	// --- session 2: reopen the same directory, verify everything survived ---
	st2, err := storage.Open(storage.Options{DataDir: dir, ValueLogFileMB: 4})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	ex2 := New(st2, transactions.New(st2, 1, nil))

	res := run(t, ex2, sess, "SELECT id, balance FROM accounts")
	if len(res.Rows) != 3 {
		t.Fatalf("after restart want 3 rows, got %d", len(res.Rows))
	}
	if got := balanceOf(res, "a2"); got != "999" {
		t.Fatalf("after restart a2 balance want 999 (the update), got %q", got)
	}
	if got := balanceOf(res, "a1"); got != "100" {
		t.Fatalf("after restart a1 balance want 100, got %q", got)
	}

	// The branch metadata survived too.
	if ok, _ := ex2.(*execImpl).branches.Exists(context.Background(), sess.Auth, "audit"); !ok {
		t.Fatal("branch 'audit' did not survive restart")
	}
}

func balanceOf(res *Result, id string) string {
	for _, row := range res.Rows {
		if row[0].Text == id {
			return row[1].Text
		}
	}
	return ""
}
