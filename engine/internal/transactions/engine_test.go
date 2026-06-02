package transactions

import (
	"context"
	"testing"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
)

func testSess(t *testing.T, ns string) auth.Session {
	t.Helper()
	s, err := auth.SessionFromClaims(&auth.SessionClaims{
		Sub: "u", Jti: "j", Role: "service",
		NamespaceAccess: []string{ns}, NamespaceID: ns,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func openStore(t *testing.T) storage.Store {
	t.Helper()
	st, err := storage.Open(storage.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestCommitVisibleAfter proves a committed write is visible to a NEW txn and
// invisible to a txn that began before the commit (snapshot isolation).
func TestCommitVisibleAfter(t *testing.T) {
	st := openStore(t)
	eng := New(st, 1, nil)
	sess := testSess(t, "tenant-a")
	ctx := context.Background()
	enc := st.Encoder()
	key := enc.RowKey(sess.Namespace, "main", "t", []byte("k1"))

	// Snapshot BEFORE the write.
	before, _ := eng.Begin(ctx, sess)

	// Write txn.
	w, _ := eng.Begin(ctx, sess)
	eng.Buffer(w, Mutation{Key: key, Value: []byte("v1")})
	if err := eng.Commit(ctx, w); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The earlier snapshot must NOT see it.
	if _, err := eng.Get(ctx, before, key); err != storage.ErrKeyNotFound {
		t.Fatalf("pre-commit snapshot should not see write, err=%v", err)
	}

	// A new snapshot must see it.
	after, _ := eng.Begin(ctx, sess)
	got, err := eng.Get(ctx, after, key)
	if err != nil || string(got) != "v1" {
		t.Fatalf("post-commit snapshot want v1, got %q err=%v", got, err)
	}
}

// TestReadYourWrites proves a txn sees its own buffered writes before commit.
func TestReadYourWrites(t *testing.T) {
	st := openStore(t)
	eng := New(st, 1, nil)
	sess := testSess(t, "tenant-a")
	ctx := context.Background()
	key := st.Encoder().RowKey(sess.Namespace, "main", "t", []byte("k"))

	tx, _ := eng.Begin(ctx, sess)
	eng.Buffer(tx, Mutation{Key: key, Value: []byte("draft")})
	got, err := eng.Get(ctx, tx, key)
	if err != nil || string(got) != "draft" {
		t.Fatalf("read-your-writes want draft, got %q err=%v", got, err)
	}
	// Delete in same txn → not found.
	eng.Buffer(tx, Mutation{Key: key, Delete: true})
	if _, err := eng.Get(ctx, tx, key); err != storage.ErrKeyNotFound {
		t.Fatalf("buffered delete should hide key, err=%v", err)
	}
}

// TestRollbackDiscards proves rollback discards buffered writes.
func TestRollbackDiscards(t *testing.T) {
	st := openStore(t)
	eng := New(st, 1, nil)
	sess := testSess(t, "tenant-a")
	ctx := context.Background()
	key := st.Encoder().RowKey(sess.Namespace, "main", "t", []byte("k"))

	tx, _ := eng.Begin(ctx, sess)
	eng.Buffer(tx, Mutation{Key: key, Value: []byte("x")})
	if err := eng.Rollback(ctx, tx); err != nil {
		t.Fatal(err)
	}

	check, _ := eng.Begin(ctx, sess)
	if _, err := eng.Get(ctx, check, key); err != storage.ErrKeyNotFound {
		t.Fatalf("rolled-back write must not persist, err=%v", err)
	}
}

// TestHLCMonotonic proves the HLC never returns a non-increasing timestamp even
// when the wall clock is frozen.
func TestHLCMonotonic(t *testing.T) {
	h := NewHLC(7)
	h.now = func() uint64 { return 1000 } // frozen wall clock
	prev := h.Now()
	for i := 0; i < 100; i++ {
		cur := h.Now()
		if cur.Encode() <= prev.Encode() {
			t.Fatalf("HLC not monotonic at %d: %d <= %d", i, cur.Encode(), prev.Encode())
		}
		prev = cur
	}
}
