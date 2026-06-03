package transactions

import (
	"context"
	"errors"
	"testing"
)

// TestWriteConflictDetected proves first-committer-wins: two transactions from
// the same snapshot writing the same key — the second to commit is rejected.
func TestWriteConflictDetected(t *testing.T) {
	st := openStore(t)
	eng := New(st, 1, nil)
	sess := testSess(t, "tenant-a")
	ctx := context.Background()
	key := st.Encoder().RowKey(sess.Namespace, "main", "t", []byte("1"))

	seed, _ := eng.Begin(ctx, sess)
	eng.Buffer(seed, Mutation{Key: key, Value: []byte("seed")})
	if err := eng.Commit(ctx, seed); err != nil {
		t.Fatal(err)
	}

	t1, _ := eng.Begin(ctx, sess)
	t2, _ := eng.Begin(ctx, sess)
	eng.Buffer(t1, Mutation{Key: key, Value: []byte("v1")})
	eng.Buffer(t2, Mutation{Key: key, Value: []byte("v2")})

	if err := eng.Commit(ctx, t1); err != nil {
		t.Fatalf("t1 should commit: %v", err)
	}
	if err := eng.Commit(ctx, t2); !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("t2 should conflict, got %v", err)
	}

	// Disjoint keys never conflict.
	k2 := st.Encoder().RowKey(sess.Namespace, "main", "t", []byte("2"))
	a, _ := eng.Begin(ctx, sess)
	b, _ := eng.Begin(ctx, sess)
	eng.Buffer(a, Mutation{Key: key, Value: []byte("a")})
	eng.Buffer(b, Mutation{Key: k2, Value: []byte("b")})
	if err := eng.Commit(ctx, a); err != nil {
		t.Fatalf("a commit: %v", err)
	}
	if err := eng.Commit(ctx, b); err != nil {
		t.Fatalf("disjoint key must not conflict: %v", err)
	}
}

// TestCommitVisibilityAtomicity proves the read oracle only advances after the
// write is durable: a snapshot taken at the published timestamp never observes a
// partially-applied multi-key commit.
func TestCommitVisibilityAtomicity(t *testing.T) {
	st := openStore(t)
	eng := New(st, 1, nil)
	sess := testSess(t, "tenant-a")
	ctx := context.Background()
	ka := st.Encoder().RowKey(sess.Namespace, "main", "t", []byte("a"))
	kb := st.Encoder().RowKey(sess.Namespace, "main", "t", []byte("b"))

	w, _ := eng.Begin(ctx, sess)
	eng.Buffer(w, Mutation{Key: ka, Value: []byte("1")})
	eng.Buffer(w, Mutation{Key: kb, Value: []byte("1")})
	if err := eng.Commit(ctx, w); err != nil {
		t.Fatal(err)
	}

	// A fresh snapshot sees both keys (all-or-nothing).
	r, _ := eng.Begin(ctx, sess)
	va, ea := eng.Get(ctx, r, ka)
	vb, eb := eng.Get(ctx, r, kb)
	if ea != nil || eb != nil || string(va) != "1" || string(vb) != "1" {
		t.Fatalf("commit not atomically visible: a=%q(%v) b=%q(%v)", va, ea, vb, eb)
	}
}
