package executor

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cloudtasticdev/basuyudb/engine/internal/parser"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// TestConcurrentInserts probes whether the engine deadlocks under concurrent
// write load (the behaviour the live psql stress test appeared to hit). N
// goroutines each insert a distinct row through the full executor path. The
// test fails if it does not complete within the deadline (deadlock) or if any
// row is lost.
func TestConcurrentInserts(t *testing.T) {
	st, _ := storage.Open(storage.Options{DataDir: t.TempDir(), ValueLogFileMB: 4})
	defer st.Close()
	ex := New(st, transactions.New(st, 1, nil))
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE t (id text PRIMARY KEY, n int)")

	const n = 25
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			stmt, perr := parser.Parse(fmt.Sprintf("INSERT INTO t (id, n) VALUES ('k%d', '%d')", i, i))
			if perr != nil {
				errs <- perr
				return
			}
			_, err := ex.Execute(context.Background(), stmt, sess, nil)
			errs <- err
		}(i)
	}

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("DEADLOCK: concurrent inserts did not complete within 30s")
	}

	errCount := 0
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			errCount++
			t.Logf("insert error: %v", err)
		}
	}

	res := run(t, ex, sess, "SELECT id FROM t")
	if len(res.Rows) != n {
		t.Fatalf("want %d rows after %d concurrent inserts (%d errored), got %d", n, n, errCount, len(res.Rows))
	}
}
