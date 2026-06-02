package executor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/cloudtasticdev/basuyudb/engine/internal/parser"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// benchDir routes BadgerDB data to BASUYUDB_BENCH_DIR (set to a roomy drive) so
// the benchmark does not exhaust a constrained system temp drive.
func benchDir(t *testing.T) string {
	base := os.Getenv("BASUYUDB_BENCH_DIR")
	if base == "" {
		base = t.TempDir()
	}
	dir := filepath.Join(base, fmt.Sprintf("bench-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func pctl(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	idx := int(float64(len(d)-1) * p)
	return d[idx]
}

// TestScaleBenchmark is a manual performance probe (run with
//
//	go test ./internal/executor -run TestScaleBenchmark -v -timeout 30m
//
// It reports honest V0.1 numbers for the single-node engine: per-row insert
// latency/throughput, batched-insert throughput, full-table-scan latency, and
// point-lookup latency (which is a FULL SCAN in V0.1 — no secondary indexes).
func TestScaleBenchmark(t *testing.T) {
	if os.Getenv("BASUYUDB_BENCH") == "" {
		t.Skip("set BASUYUDB_BENCH=1 to run the scale benchmark")
	}

	st, err := storage.Open(storage.Options{DataDir: benchDir(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	txn := transactions.New(st, 1, nil)
	ex := New(st, txn)
	sess := testSession(t)
	ctx := context.Background()

	mustExec := func(sql string) {
		stmt, err := parser.Parse(sql)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ex.Execute(ctx, stmt, sess, nil); err != nil {
			t.Fatal(err)
		}
	}

	mustExec("CREATE TABLE bench (id text PRIMARY KEY, name text NOT NULL, amount int)")

	// --- per-row INSERT latency/throughput (each is its own txn + Flush) ---
	const nRows = 100_000
	insStmt := make([]string, 8)
	lat := make([]time.Duration, 0, 20_000)
	t0 := time.Now()
	for i := 0; i < nRows; i++ {
		sql := fmt.Sprintf("INSERT INTO bench (id, name, amount) VALUES ('id-%07d', 'user %d', '%d')", i, i, i*3%100000)
		stmt, err := parser.Parse(sql)
		if err != nil {
			t.Fatal(err)
		}
		s := time.Now()
		if _, err := ex.Execute(ctx, stmt, sess, nil); err != nil {
			t.Fatal(err)
		}
		if i%5 == 0 { // sample latency
			lat = append(lat, time.Since(s))
		}
	}
	insElapsed := time.Since(t0)
	_ = insStmt

	t.Logf("INSERT  %d rows in %v  →  %.0f rows/sec", nRows, insElapsed.Round(time.Millisecond), float64(nRows)/insElapsed.Seconds())
	t.Logf("INSERT  per-row latency  p50=%v  p99=%v  p999=%v",
		pctl(lat, 0.50).Round(time.Microsecond), pctl(lat, 0.99).Round(time.Microsecond), pctl(lat, 0.999).Round(time.Microsecond))

	// --- full table scan (SELECT *) ---
	scanStmt, _ := parser.Parse("SELECT id, name, amount FROM bench")
	ts := time.Now()
	res, err := ex.Execute(ctx, scanStmt, sess, nil)
	if err != nil {
		t.Fatal(err)
	}
	scanDur := time.Since(ts)
	if len(res.Rows) != nRows {
		t.Fatalf("scan returned %d rows, want %d", len(res.Rows), nRows)
	}
	t.Logf("SCAN    full %d-row table in %v  →  %.1fM rows/sec", nRows, scanDur.Round(time.Millisecond), float64(nRows)/scanDur.Seconds()/1e6)

	// --- point lookup (WHERE id = X) — PK point-get fast path (V0.2 planner) ---
	lookups := []string{"id-0000000", "id-0050000", "id-0099999"}
	var plat []time.Duration
	for r := 0; r < 30; r++ {
		for _, id := range lookups {
			stmt, _ := parser.Parse(fmt.Sprintf("SELECT amount FROM bench WHERE id = '%s'", id))
			s := time.Now()
			rr, err := ex.Execute(ctx, stmt, sess, nil)
			if err != nil || len(rr.Rows) != 1 {
				t.Fatalf("lookup %s failed: rows=%d err=%v", id, len(rr.Rows), err)
			}
			plat = append(plat, time.Since(s))
		}
	}
	t.Logf("LOOKUP  WHERE id=X (PK point-get, table=%d rows)  p50=%v  p99=%v",
		nRows, pctl(plat, 0.50).Round(time.Microsecond), pctl(plat, 0.99).Round(time.Microsecond))

	// --- indexed range scan (WHERE amount BETWEEN-equivalent) vs full scan ---
	// Build a secondary index on amount, then time a narrow range query. Without
	// the index this is a full 100k-row scan; with it, a bounded index range.
	mustExec("CREATE INDEX idx_amount ON bench (amount)")
	var rlat []time.Duration
	for r := 0; r < 30; r++ {
		lo := (r * 131) % 99000
		stmt, _ := parser.Parse(fmt.Sprintf("SELECT id FROM bench WHERE amount >= %d AND amount <= %d", lo, lo+50))
		s := time.Now()
		if _, err := ex.Execute(ctx, stmt, sess, nil); err != nil {
			t.Fatal(err)
		}
		rlat = append(rlat, time.Since(s))
	}
	t.Logf("RANGE   WHERE amount in [x,x+50] (indexed, table=%d rows)  p50=%v  p99=%v",
		nRows, pctl(rlat, 0.50).Round(time.Microsecond), pctl(rlat, 0.99).Round(time.Microsecond))

	// ORDER BY amount DESC LIMIT 10 served by the index (no full sort).
	var olat []time.Duration
	for r := 0; r < 30; r++ {
		stmt, _ := parser.Parse("SELECT id FROM bench ORDER BY amount DESC LIMIT 10")
		s := time.Now()
		rr, err := ex.Execute(ctx, stmt, sess, nil)
		if err != nil || len(rr.Rows) != 10 {
			t.Fatalf("order-by-limit failed: rows=%d err=%v", len(rr.Rows), err)
		}
		olat = append(olat, time.Since(s))
	}
	t.Logf("TOPN    ORDER BY amount DESC LIMIT 10 (indexed, table=%d rows)  p50=%v  p99=%v",
		nRows, pctl(olat, 0.50).Round(time.Microsecond), pctl(olat, 0.99).Round(time.Microsecond))

	// --- batched insert throughput (many rows, ONE transaction) ---
	const batchN = 50_000
	bt := time.Now()
	tx, _ := txn.Begin(ctx, sess.Auth)
	enc := st.Encoder()
	for i := 0; i < batchN; i++ {
		cells := []Datum{{Text: fmt.Sprintf("b-%07d", i)}, {Text: "x"}, {Text: fmt.Sprintf("%d", i)}}
		key := enc.RowKey(sess.Namespace(), "main", "bench", []byte(fmt.Sprintf("b-%07d", i)))
		txn.Buffer(tx, transactions.Mutation{Key: key, Value: encodeRow(cells)})
	}
	if err := txn.Commit(ctx, tx); err != nil {
		t.Fatal(err)
	}
	batchDur := time.Since(bt)
	t.Logf("BATCH   %d rows in ONE txn in %v  →  %.0f rows/sec", batchN, batchDur.Round(time.Millisecond), float64(batchN)/batchDur.Seconds())
}
