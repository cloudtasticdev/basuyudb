package executor

import "testing"

// TestWindowFunctions covers ROW_NUMBER / RANK / DENSE_RANK and aggregate-OVER
// with PARTITION BY and ORDER BY.
func TestWindowFunctions(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE sales (id INT PRIMARY KEY, dept TEXT, amt INT)")
	rows := []struct {
		id   int
		dept string
		amt  int
	}{
		{1, "a", 10}, {2, "a", 30}, {3, "a", 30}, {4, "b", 20}, {5, "b", 50},
	}
	for _, r := range rows {
		run(t, ex, sess, "INSERT INTO sales (id, dept, amt) VALUES ("+itoaInt(r.id)+", '"+r.dept+"', "+itoaInt(r.amt)+")")
	}

	// ROW_NUMBER over the whole set ordered by id.
	r := run(t, ex, sess, "SELECT id, row_number() OVER (ORDER BY id) FROM sales ORDER BY id")
	want := []string{"1", "2", "3", "4", "5"}
	for i, w := range want {
		if r.Rows[i][1].Text != w {
			t.Fatalf("row_number row %d: want %s, got %s", i, w, r.Rows[i][1].Text)
		}
	}

	// PARTITION BY dept, ROW_NUMBER ordered by id within each dept.
	r = run(t, ex, sess, "SELECT id, row_number() OVER (PARTITION BY dept ORDER BY id) FROM sales ORDER BY id")
	wantRN := []string{"1", "2", "3", "1", "2"} // a:1,2,3  b:1,2
	for i, w := range wantRN {
		if r.Rows[i][1].Text != w {
			t.Fatalf("partitioned row_number row %d: want %s, got %s", i, w, r.Rows[i][1].Text)
		}
	}

	// RANK / DENSE_RANK with a tie (dept a has two amt=30).
	r = run(t, ex, sess, "SELECT id, rank() OVER (PARTITION BY dept ORDER BY amt), dense_rank() OVER (PARTITION BY dept ORDER BY amt) FROM sales ORDER BY id")
	// dept a ordered by amt: id1(10)->rank1, id2(30)->rank2, id3(30)->rank2; dense 1,2,2
	wantRank := []string{"1", "2", "2", "1", "2"}
	wantDense := []string{"1", "2", "2", "1", "2"}
	for i := range wantRank {
		if r.Rows[i][1].Text != wantRank[i] {
			t.Fatalf("rank row %d: want %s, got %s", i, wantRank[i], r.Rows[i][1].Text)
		}
		if r.Rows[i][2].Text != wantDense[i] {
			t.Fatalf("dense_rank row %d: want %s, got %s", i, wantDense[i], r.Rows[i][2].Text)
		}
	}

	// SUM OVER (PARTITION BY dept) — whole-partition total on every row.
	r = run(t, ex, sess, "SELECT id, sum(amt) OVER (PARTITION BY dept) FROM sales ORDER BY id")
	// dept a total = 70, dept b total = 70
	wantSum := []string{"70", "70", "70", "70", "70"}
	for i, w := range wantSum {
		if r.Rows[i][1].Text != w {
			t.Fatalf("sum over partition row %d: want %s, got %s", i, w, r.Rows[i][1].Text)
		}
	}

	// COUNT(*) OVER (PARTITION BY dept).
	r = run(t, ex, sess, "SELECT id, count(*) OVER (PARTITION BY dept) FROM sales ORDER BY id")
	wantCnt := []string{"3", "3", "3", "2", "2"}
	for i, w := range wantCnt {
		if r.Rows[i][1].Text != w {
			t.Fatalf("count(*) over partition row %d: want %s, got %s", i, w, r.Rows[i][1].Text)
		}
	}
}
