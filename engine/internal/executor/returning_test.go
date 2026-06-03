package executor

import (
	"fmt"
	"testing"
)

func TestReturning(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)
	run(t, ex, sess, "CREATE TABLE t (id INT PRIMARY KEY, n INT, label TEXT)")

	// INSERT ... RETURNING — the operation every ORM needs to get the new row.
	r := run(t, ex, sess, "INSERT INTO t (id, n, label) VALUES (1, 10, 'a') RETURNING id, label")
	if len(r.Rows) != 1 || r.Rows[0][0].Text != "1" || r.Rows[0][1].Text != "a" {
		t.Fatalf("INSERT RETURNING id,label want [1 a], got %#v", r.Rows)
	}
	if r.Columns[0].Name != "id" || r.Columns[1].Name != "label" {
		t.Fatalf("RETURNING column names wrong: %#v", r.Columns)
	}

	// RETURNING *
	r2 := run(t, ex, sess, "INSERT INTO t (id, n, label) VALUES (2, 20, 'b') RETURNING *")
	if len(r2.Rows) != 1 || len(r2.Rows[0]) != 3 || r2.Rows[0][1].Text != "20" {
		t.Fatalf("INSERT RETURNING * want full row, got %#v", r2.Rows)
	}

	// UPDATE ... RETURNING returns the new values for each affected row.
	for i := 3; i <= 5; i++ {
		run(t, ex, sess, fmt.Sprintf("INSERT INTO t (id, n, label) VALUES (%d, %d, 'x')", i, i*10))
	}
	ru := run(t, ex, sess, "UPDATE t SET label = 'updated' WHERE n >= 30 RETURNING id, label")
	if len(ru.Rows) != 3 {
		t.Fatalf("UPDATE RETURNING want 3 rows, got %d", len(ru.Rows))
	}
	if ru.Rows[0][1].Text != "updated" {
		t.Fatalf("UPDATE RETURNING should show new value, got %q", ru.Rows[0][1].Text)
	}

	// DELETE ... RETURNING returns the removed rows.
	rd := run(t, ex, sess, "DELETE FROM t WHERE id = 1 RETURNING id, n")
	if len(rd.Rows) != 1 || rd.Rows[0][0].Text != "1" || rd.Rows[0][1].Text != "10" {
		t.Fatalf("DELETE RETURNING want [1 10], got %#v", rd.Rows)
	}
}
