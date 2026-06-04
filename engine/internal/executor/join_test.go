package executor

import (
	"testing"

	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// TestGate3OTelJoin is the Gate-3 acceptance: a SQL JOIN across the otel_spans
// built-in table and a relational table, correlated by a JSONB ->> extraction
// from the span attributes. This is the query no other database can run in one
// statement against one engine.
func TestGate3OTelJoin(t *testing.T) {
	st, _ := storage.Open(storage.Options{DataDir: t.TempDir(), ValueLogFileMB: 4})
	defer st.Close()
	ex := New(st, transactions.New(st, 1, nil))
	sess := testSession(t)

	// Relational users table.
	run(t, ex, sess, "CREATE TABLE users (id text PRIMARY KEY, email text NOT NULL)")
	run(t, ex, sess, "INSERT INTO users (id, email) VALUES ('u1', 'alice@acme.com')")
	run(t, ex, sess, "INSERT INTO users (id, email) VALUES ('u2', 'bob@acme.com')")

	// OTel spans (populated via SQL here; the OTLP receiver uses IngestSpans).
	run(t, ex, sess, `INSERT INTO otel_spans
		(trace_id, span_id, parent_span_id, service_name, span_name, duration_ms, status, started_at, attributes)
		VALUES ('t1','s1','','auth','login','12','ERROR','2026-06-01T00:00:00Z','{"user_id":"u1","ip":"1.2.3.4"}')`)
	run(t, ex, sess, `INSERT INTO otel_spans
		(trace_id, span_id, parent_span_id, service_name, span_name, duration_ms, status, started_at, attributes)
		VALUES ('t2','s2','','auth','login','8','OK','2026-06-01T00:00:01Z','{"user_id":"u2"}')`)

	// THE DEMO QUERY: correlate error spans to the user who triggered them.
	q := `SELECT s.trace_id, s.service_name, u.email
	 FROM otel_spans s JOIN users u ON u.id = s.attributes ->> 'user_id'
	 WHERE s.status = 'ERROR'`
	res := run(t, ex, sess, q)

	if len(res.Rows) != 1 {
		t.Fatalf("want 1 correlated error row, got %d: %#v", len(res.Rows), res.Rows)
	}
	row := res.Rows[0]
	if row[0].Text != "t1" || row[1].Text != "auth" || row[2].Text != "alice@acme.com" {
		t.Fatalf("unexpected join row: trace=%q service=%q email=%q", row[0].Text, row[1].Text, row[2].Text)
	}
}

// TestJSONBExtraction unit-tests the ->> / -> / #>> operators.
func TestJSONBExtraction(t *testing.T) {
	doc := value{text: `{"a":{"b":"deep"},"n":42,"arr":[10,20]}`, oid: OIDText}

	got, _ := jsonbExtract("->>", doc, value{text: "n"})
	if got.text != "42" {
		t.Fatalf("->> n want 42, got %q", got.text)
	}
	got, _ = jsonbExtract("#>>", doc, value{text: "{a,b}"})
	if got.text != "deep" {
		t.Fatalf("#>> {a,b} want deep, got %q", got.text)
	}
	got, _ = jsonbExtract("->>", doc, value{text: "missing"})
	if !got.null {
		t.Fatalf("->> missing want NULL, got %q", got.text)
	}
}

// TestRightJoin verifies RIGHT OUTER JOIN emits NULL-filled left columns for
// unmatched right rows.
func TestRightJoin(t *testing.T) {
	st, _ := storage.Open(storage.Options{DataDir: t.TempDir(), ValueLogFileMB: 4})
	defer st.Close()
	ex := New(st, transactions.New(st, 1, nil))
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE a (id text PRIMARY KEY)")
	run(t, ex, sess, "CREATE TABLE b (id text PRIMARY KEY, a_id text)")
	run(t, ex, sess, "INSERT INTO b (id, a_id) VALUES ('b1', 'a1')")
	// no matching a row

	res := run(t, ex, sess, "SELECT a.id, b.id FROM a RIGHT JOIN b ON a.id = b.a_id")
	if len(res.Rows) != 1 {
		t.Fatalf("want 1 row, got %d: %#v", len(res.Rows), res.Rows)
	}
	if !res.Rows[0][0].Null || res.Rows[0][1].Text != "b1" {
		t.Fatalf("want (NULL, b1), got (null=%v, %q)", res.Rows[0][0].Null, res.Rows[0][1].Text)
	}
}

// TestFullOuterJoin verifies FULL OUTER JOIN emits all rows from both sides,
// NULL-filling the missing side.
func TestFullOuterJoin(t *testing.T) {
	st, _ := storage.Open(storage.Options{DataDir: t.TempDir(), ValueLogFileMB: 4})
	defer st.Close()
	ex := New(st, transactions.New(st, 1, nil))
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE a (id text PRIMARY KEY)")
	run(t, ex, sess, "CREATE TABLE b (id text PRIMARY KEY, a_id text)")
	run(t, ex, sess, "INSERT INTO a (id) VALUES ('a1')")
	run(t, ex, sess, "INSERT INTO a (id) VALUES ('a2')")
	run(t, ex, sess, "INSERT INTO b (id, a_id) VALUES ('b1', 'a1')")
	run(t, ex, sess, "INSERT INTO b (id, a_id) VALUES ('b2', 'a99')") // unmatched right

	res := run(t, ex, sess, "SELECT a.id, b.id FROM a FULL JOIN b ON a.id = b.a_id")
	if len(res.Rows) != 3 {
		// Expected: (a1,b1), (a2,NULL), (NULL,b2)
		t.Fatalf("want 3 rows (a1/b1 match, a2 unmatched left, b2 unmatched right), got %d: %#v", len(res.Rows), res.Rows)
	}
}

// TestGenerateSeries verifies generate_series as a table-valued function.
func TestGenerateSeries(t *testing.T) {
	st, _ := storage.Open(storage.Options{DataDir: t.TempDir(), ValueLogFileMB: 4})
	defer st.Close()
	ex := New(st, transactions.New(st, 1, nil))
	sess := testSession(t)

	// Basic range 1..5
	res := run(t, ex, sess, "SELECT generate_series FROM generate_series(1, 5)")
	if len(res.Rows) != 5 {
		t.Fatalf("want 5 rows, got %d: %#v", len(res.Rows), res.Rows)
	}
	if res.Rows[0][0].Text != "1" || res.Rows[4][0].Text != "5" {
		t.Fatalf("unexpected values: first=%q last=%q", res.Rows[0][0].Text, res.Rows[4][0].Text)
	}

	// Step=2
	res = run(t, ex, sess, "SELECT generate_series FROM generate_series(0, 6, 2)")
	if len(res.Rows) != 4 { // 0,2,4,6
		t.Fatalf("want 4 rows (step 2), got %d: %#v", len(res.Rows), res.Rows)
	}

	// Alias
	res = run(t, ex, sess, "SELECT n FROM generate_series(1, 3) AS gs(n)")
	if len(res.Rows) != 3 {
		t.Fatalf("want 3 rows with alias, got %d", len(res.Rows))
	}
}

// TestLeftJoinNullFill verifies LEFT JOIN emits NULL-filled right columns.
func TestLeftJoinNullFill(t *testing.T) {
	st, _ := storage.Open(storage.Options{DataDir: t.TempDir(), ValueLogFileMB: 4})
	defer st.Close()
	ex := New(st, transactions.New(st, 1, nil))
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE a (id text PRIMARY KEY)")
	run(t, ex, sess, "CREATE TABLE b (id text PRIMARY KEY, a_id text)")
	run(t, ex, sess, "INSERT INTO a (id) VALUES ('a1')")
	// no matching b row

	res := run(t, ex, sess, "SELECT a.id, b.id FROM a LEFT JOIN b ON b.a_id = a.id")
	if len(res.Rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(res.Rows))
	}
	if res.Rows[0][0].Text != "a1" || !res.Rows[0][1].Null {
		t.Fatalf("want (a1, NULL), got (%q, null=%v)", res.Rows[0][0].Text, res.Rows[0][1].Null)
	}
}
