package executor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/parser"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// quietLogger discards storage/engine logs so benchmark output stays clean.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// benchSession builds a session without a *testing.T (usable from Benchmarks).
func benchSession() *session.Session {
	s, err := auth.SessionFromClaims(&auth.SessionClaims{
		Sub: "u", Jti: "j", Role: "service",
		NamespaceAccess: []string{"tenant-a"}, NamespaceID: "tenant-a",
	})
	if err != nil {
		panic(err)
	}
	return session.New(s, 1, nil)
}

// benchSetup opens a fresh engine, seeds a table with n rows, and returns the
// executor + session. The table: bench(id text PK, name text, amount int) with a
// secondary index on amount.
func benchSetup(b *testing.B, n int, withIndex bool) (Executor, *session.Session) {
	b.Helper()
	st, err := storage.Open(storage.Options{DataDir: b.TempDir(), ValueLogFileMB: 16, Logger: quietLogger()})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { st.Close() })
	txn := transactions.New(st, 1, nil)
	ex := New(st, txn)
	sess := benchSession()
	ctx := context.Background()
	exec := func(sql string) {
		stmt, err := parser.Parse(sql)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := ex.Execute(ctx, stmt, sess, nil); err != nil {
			b.Fatal(err)
		}
	}
	exec("CREATE TABLE bench (id text PRIMARY KEY, name text NOT NULL, amount int)")
	// Bulk-seed in one transaction for fast setup.
	tx, _ := txn.Begin(ctx, sess.Auth)
	enc := st.Encoder()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("id-%07d", i)
		cells := []Datum{{Text: id}, {Text: fmt.Sprintf("user %d", i)}, {Text: fmt.Sprintf("%d", i*3%100000)}}
		key := enc.RowKey(sess.Namespace(), "main", "bench", []byte(id))
		txn.Buffer(tx, transactions.Mutation{Key: key, Value: encodeRow(cells)})
	}
	if err := txn.Commit(ctx, tx); err != nil {
		b.Fatal(err)
	}
	if withIndex {
		exec("CREATE INDEX idx_amount ON bench (amount)")
	}
	return ex, sess
}

// parseOnce returns a parsed statement, failing the benchmark on error.
func parseOnce(b *testing.B, sql string) ast.Node {
	b.Helper()
	stmt, err := parser.Parse(sql)
	if err != nil {
		b.Fatal(err)
	}
	return stmt
}

// BenchmarkParseSelect measures pure parse cost of a representative query.
func BenchmarkParseSelect(b *testing.B) {
	sql := "SELECT amount FROM bench WHERE id = 'id-0050000'"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := parser.Parse(sql); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPointLookupPK_Exec isolates the EXECUTE cost of a PK point-get
// (statement pre-parsed) against a 100k-row table.
func BenchmarkPointLookupPK_Exec(b *testing.B) {
	ex, sess := benchSetup(b, 100_000, false)
	stmt := parseOnce(b, "SELECT amount FROM bench WHERE id = 'id-0050000'")
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, err := ex.Execute(ctx, stmt, sess, nil)
		if err != nil || len(r.Rows) != 1 {
			b.Fatalf("rows=%d err=%v", len(r.Rows), err)
		}
	}
}

// BenchmarkPointLookupPK_ParseExec measures the full parse+execute path (what a
// simple-query client pays per request).
func BenchmarkPointLookupPK_ParseExec(b *testing.B) {
	ex, sess := benchSetup(b, 100_000, false)
	sql := "SELECT amount FROM bench WHERE id = 'id-0050000'"
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stmt, err := parser.Parse(sql)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := ex.Execute(ctx, stmt, sess, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIndexedRange measures a bounded indexed range scan.
func BenchmarkIndexedRange(b *testing.B) {
	ex, sess := benchSetup(b, 100_000, true)
	stmt := parseOnce(b, "SELECT id FROM bench WHERE amount >= 40000 AND amount <= 40050")
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ex.Execute(ctx, stmt, sess, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSeqScan1k measures a full scan over a 1,000-row table.
func BenchmarkSeqScan1k(b *testing.B) {
	ex, sess := benchSetup(b, 1_000, false)
	stmt := parseOnce(b, "SELECT id, name, amount FROM bench")
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, err := ex.Execute(ctx, stmt, sess, nil)
		if err != nil || len(r.Rows) != 1000 {
			b.Fatalf("rows=%d err=%v", len(r.Rows), err)
		}
	}
}

// BenchmarkInsertOne measures a single-row autocommit INSERT (own txn + flush).
func BenchmarkInsertOne(b *testing.B) {
	ex, sess := benchSetup(b, 0, false)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sql := fmt.Sprintf("INSERT INTO bench (id, name, amount) VALUES ('k-%09d','n','%d')", i, i)
		stmt, err := parser.Parse(sql)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := ex.Execute(ctx, stmt, sess, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRLSOverhead_On vs _Off measures the per-query cost RLS adds: same
// point query against a table with an enabled USING policy vs a plain table.
func BenchmarkRLSOverhead_Off(b *testing.B) {
	ex, sess := benchSetup(b, 10_000, false)
	stmt := parseOnce(b, "SELECT amount FROM bench WHERE id = 'id-0005000'")
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ex.Execute(ctx, stmt, sess, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRLSOverhead_On(b *testing.B) {
	ex, sess := benchSetup(b, 10_000, false)
	ctx := context.Background()
	for _, s := range []string{
		"ALTER TABLE bench ENABLE ROW LEVEL SECURITY",
		"CREATE POLICY pol ON bench USING (name = name)", // always-true, but exercises the predicate path
	} {
		stmt := parseOnce(b, s)
		if _, err := ex.Execute(ctx, stmt, sess, nil); err != nil {
			b.Fatal(err)
		}
	}
	stmt := parseOnce(b, "SELECT amount FROM bench WHERE id = 'id-0005000'")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ex.Execute(ctx, stmt, sess, nil); err != nil {
			b.Fatal(err)
		}
	}
}
