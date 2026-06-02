package executor

import (
	"context"
	"testing"
	"time"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// sessOn returns a session bound to a specific branch.
func sessOn(t *testing.T, branch string) *session.Session {
	t.Helper()
	a, err := auth.DevSession("tenant-a", branch)
	if err != nil {
		t.Fatal(err)
	}
	return session.New(a, 1, nil)
}

// TestGate2BranchCOW is the Gate-2 acceptance: branch create is fast, writes on
// a feature branch are invisible to main, the branch sees main's data via COW
// fall-through, and MERGE applies the branch's changes back to main.
func TestGate2BranchCOW(t *testing.T) {
	st, _ := storage.Open(storage.Options{DataDir: t.TempDir(), ValueLogFileMB: 4})
	defer st.Close()
	ex := New(st, transactions.New(st, 1, nil))
	ctx := context.Background()

	main := sessOn(t, "main")
	run(t, ex, main, "CREATE TABLE products (id text PRIMARY KEY, price int)")
	run(t, ex, main, "INSERT INTO products (id, price) VALUES ('p1', '100')")
	run(t, ex, main, "INSERT INTO products (id, price) VALUES ('p2', '200')")

	// CREATE BRANCH must be O(1)/fast (Gate-2 <500ms; here vastly under).
	start := time.Now()
	run(t, ex, main, "CREATE BRANCH feature FROM main")
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("CREATE BRANCH took %v (>500ms)", d)
	}

	feat := sessOn(t, "feature")

	// The branch sees main's data via COW fall-through.
	if got := run(t, ex, feat, "SELECT * FROM products"); len(got.Rows) != 2 {
		t.Fatalf("branch should see 2 inherited rows, got %d", len(got.Rows))
	}

	// Mutate on the branch: add p3, change p1, delete p2.
	run(t, ex, feat, "INSERT INTO products (id, price) VALUES ('p3', '300')")
	run(t, ex, feat, "UPDATE products SET price = '111' WHERE id = 'p1'")
	run(t, ex, feat, "DELETE FROM products WHERE id = 'p2'")

	// Branch view: p1=111, p3=300, p2 gone → 2 rows.
	fv := run(t, ex, feat, "SELECT id, price FROM products")
	if len(fv.Rows) != 2 {
		t.Fatalf("branch view want 2 rows, got %d: %#v", len(fv.Rows), fv.Rows)
	}
	if v := priceOf(fv, "p1"); v != "111" {
		t.Fatalf("branch p1 want 111, got %q", v)
	}

	// MAIN is untouched: p1=100, p2=200, no p3 → 2 rows, p1 still 100.
	mv := run(t, ex, main, "SELECT id, price FROM products")
	if len(mv.Rows) != 2 {
		t.Fatalf("main should be unchanged (2 rows), got %d", len(mv.Rows))
	}
	if v := priceOf(mv, "p1"); v != "100" {
		t.Fatalf("main p1 must still be 100, got %q", v)
	}

	// MERGE the branch into main.
	run(t, ex, main, "MERGE BRANCH feature INTO main")

	// Main now reflects the branch: p1=111, p2 deleted, p3=300 → 2 rows.
	after := run(t, ex, main, "SELECT id, price FROM products")
	if len(after.Rows) != 2 {
		t.Fatalf("after merge main want 2 rows, got %d: %#v", len(after.Rows), after.Rows)
	}
	if v := priceOf(after, "p1"); v != "111" {
		t.Fatalf("after merge p1 want 111, got %q", v)
	}
	if priceOf(after, "p3") != "300" {
		t.Fatal("after merge p3 should be present")
	}
	if priceOf(after, "p2") != "" {
		t.Fatal("after merge p2 should be deleted")
	}

	// DROP the branch.
	run(t, ex, main, "DROP BRANCH feature")
	if ok, _ := ex.(*execImpl).branches.Exists(ctx, main.Auth, "feature"); ok {
		t.Fatal("branch should not exist after DROP")
	}
}

func priceOf(res *Result, id string) string {
	for _, row := range res.Rows {
		if row[0].Text == id {
			return row[1].Text
		}
	}
	return ""
}
