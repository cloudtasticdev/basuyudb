package executor

import "testing"

// TestSetOperations covers UNION/UNION ALL/INTERSECT/EXCEPT with the combined
// ORDER BY / LIMIT applying to the whole result.
func TestSetOperations(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE a (id INT PRIMARY KEY, v INT)")
	run(t, ex, sess, "CREATE TABLE b (id INT PRIMARY KEY, v INT)")
	// a.v = {1,2,2,3} (each row a unique id); b.v = {2,3,4}
	run(t, ex, sess, "INSERT INTO a (id, v) VALUES (1, 1)")
	run(t, ex, sess, "INSERT INTO a (id, v) VALUES (2, 2)")
	run(t, ex, sess, "INSERT INTO a (id, v) VALUES (3, 2)")
	run(t, ex, sess, "INSERT INTO a (id, v) VALUES (4, 3)")
	run(t, ex, sess, "INSERT INTO b (id, v) VALUES (1, 2)")
	run(t, ex, sess, "INSERT INTO b (id, v) VALUES (2, 3)")
	run(t, ex, sess, "INSERT INTO b (id, v) VALUES (3, 4)")

	// UNION dedups: {1,2,3,4}.
	r := run(t, ex, sess, "SELECT v FROM a UNION SELECT v FROM b ORDER BY v")
	if got := col0(r); !eqInts(got, []string{"1", "2", "3", "4"}) {
		t.Fatalf("UNION: want [1 2 3 4], got %v", got)
	}
	// UNION ALL keeps duplicates: a has 1,2,2,3 + b 2,3,4 -> 7 rows.
	r = run(t, ex, sess, "SELECT v FROM a UNION ALL SELECT v FROM b")
	if len(r.Rows) != 7 {
		t.Fatalf("UNION ALL: want 7 rows, got %d", len(r.Rows))
	}
	// INTERSECT: values in both = {2,3}.
	r = run(t, ex, sess, "SELECT v FROM a INTERSECT SELECT v FROM b ORDER BY v")
	if got := col0(r); !eqInts(got, []string{"2", "3"}) {
		t.Fatalf("INTERSECT: want [2 3], got %v", got)
	}
	// EXCEPT: a-values not in b = {1}.
	r = run(t, ex, sess, "SELECT v FROM a EXCEPT SELECT v FROM b ORDER BY v")
	if got := col0(r); !eqInts(got, []string{"1"}) {
		t.Fatalf("EXCEPT: want [1], got %v", got)
	}
	// Combined ORDER BY + LIMIT.
	r = run(t, ex, sess, "SELECT v FROM a UNION SELECT v FROM b ORDER BY v DESC LIMIT 2")
	if got := col0(r); !eqInts(got, []string{"4", "3"}) {
		t.Fatalf("UNION ORDER BY DESC LIMIT 2: want [4 3], got %v", got)
	}
}

func col0(r *Result) []string {
	out := make([]string, len(r.Rows))
	for i, row := range r.Rows {
		out[i] = row[0].Text
	}
	return out
}

func eqInts(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
