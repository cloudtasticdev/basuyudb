package executor

import (
	"strings"
	"testing"
)

// TestForeignKey covers child-side referential integrity on INSERT and
// parent-side RESTRICT on DELETE.
func TestForeignKey(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE authors (id INT PRIMARY KEY, name TEXT)")
	run(t, ex, sess, "CREATE TABLE books (id INT PRIMARY KEY, author_id INT REFERENCES authors(id), title TEXT)")
	run(t, ex, sess, "INSERT INTO authors (id, name) VALUES (1, 'Ada')")

	// Child insert referencing an existing parent succeeds.
	run(t, ex, sess, "INSERT INTO books (id, author_id, title) VALUES (10, 1, 'Notes')")

	// Child insert referencing a missing parent is rejected.
	if err := execErr(ex, sess, "INSERT INTO books (id, author_id, title) VALUES (11, 99, 'Ghost')"); err == nil {
		t.Fatalf("expected FK violation for missing parent")
	} else if !strings.Contains(err.Error(), "foreign key") && !strings.Contains(err.Error(), "23503") {
		t.Fatalf("expected FK error, got %v", err)
	}

	// NULL FK is allowed (optional relationship).
	run(t, ex, sess, "INSERT INTO books (id, title) VALUES (12, 'Orphan-ok')")

	// Deleting a referenced parent is blocked (RESTRICT).
	if err := execErr(ex, sess, "DELETE FROM authors WHERE id = 1"); err == nil {
		t.Fatalf("expected FK parent-side RESTRICT on delete")
	} else if !strings.Contains(err.Error(), "foreign key") && !strings.Contains(err.Error(), "23503") {
		t.Fatalf("expected FK error on delete, got %v", err)
	}

	// After removing the child, the parent can be deleted.
	run(t, ex, sess, "DELETE FROM books WHERE author_id = 1")
	run(t, ex, sess, "DELETE FROM authors WHERE id = 1")
}

// TestCheckConstraint covers CHECK enforcement on INSERT and UPDATE, including
// the NULL-passes rule.
func TestCheckConstraint(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE items (id INT PRIMARY KEY, qty INT CHECK (qty > 0), kind TEXT CHECK (kind <> ''))")

	// Valid row.
	run(t, ex, sess, "INSERT INTO items (id, qty, kind) VALUES (1, 5, 'a')")

	// qty <= 0 violates the check.
	if err := execErr(ex, sess, "INSERT INTO items (id, qty, kind) VALUES (2, 0, 'b')"); err == nil {
		t.Fatalf("expected CHECK violation for qty=0")
	} else if !strings.Contains(err.Error(), "check") && !strings.Contains(err.Error(), "23514") {
		t.Fatalf("expected CHECK error, got %v", err)
	}

	// Empty kind violates the check.
	if err := execErr(ex, sess, "INSERT INTO items (id, qty, kind) VALUES (3, 1, '')"); err == nil {
		t.Fatalf("expected CHECK violation for empty kind")
	}

	// NULL qty passes (CHECK fails only on FALSE).
	run(t, ex, sess, "INSERT INTO items (id, kind) VALUES (4, 'c')")

	// UPDATE that violates the check is rejected.
	if err := execErr(ex, sess, "UPDATE items SET qty = -1 WHERE id = 1"); err == nil {
		t.Fatalf("expected CHECK violation on UPDATE")
	}

	// Valid UPDATE succeeds.
	run(t, ex, sess, "UPDATE items SET qty = 10 WHERE id = 1")
	r := run(t, ex, sess, "SELECT qty FROM items WHERE id = 1")
	if r.Rows[0][0].Text != "10" {
		t.Fatalf("want qty=10 after update, got %q", r.Rows[0][0].Text)
	}
}
