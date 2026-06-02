package storage

import (
	"bytes"
	"context"
	"testing"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
)

// mustNS builds a validated NamespaceID for tests via the auth package (the
// only legitimate construction path).
func mustNS(t *testing.T, raw string) auth.NamespaceID {
	t.Helper()
	s, err := auth.SessionFromClaims(&auth.SessionClaims{
		Sub: "test-user",
		Jti: "jti-1",
		Role: "service",
		NamespaceAccess: []string{raw},
		NamespaceID: raw,
	})
	if err != nil {
		t.Fatalf("build namespace %q: %v", raw, err)
	}
	return s.Namespace
}

func openTemp(t *testing.T) Store {
	t.Helper()
	st, err := Open(Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open managed store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestManagedWriteReadRoundTrip proves the managed-mode write/read path:
// a WriteBatch committed at commitTs is visible to a read transaction opened at
// a later readTs, and absent to a read transaction opened before the commit.
func TestManagedWriteReadRoundTrip(t *testing.T) {
	st := openTemp(t)
	ns := mustNS(t, "tenant-a")
	enc := st.Encoder()

	key := enc.RowKey(ns, "main", "users", []byte("pk-1"))
	val := []byte(`{"id":"pk-1","email":"a@example.com"}`)

	wb := st.NewWriteBatchAt(10)
	if err := wb.Set(key, val); err != nil {
		t.Fatalf("write batch set: %v", err)
	}
	if err := wb.Flush(); err != nil {
		t.Fatalf("write batch flush: %v", err)
	}

	// Read at a timestamp after the commit: value visible.
	rtx := st.NewTransactionAt(20)
	got, err := rtx.Get(key)
	rtx.Discard()
	if err != nil {
		t.Fatalf("get at readTs=20: %v", err)
	}
	if !bytes.Equal(got, val) {
		t.Fatalf("value mismatch: got %q want %q", got, val)
	}

	// Read at a timestamp before the commit: not visible (snapshot isolation).
	rtx2 := st.NewTransactionAt(5)
	_, err = rtx2.Get(key)
	rtx2.Discard()
	if err != ErrKeyNotFound {
		t.Fatalf("expected ErrKeyNotFound at readTs=5, got %v", err)
	}
}

// TestRowPrefixScan proves RowKey lives under RowPrefix so a prefix iterator
// enumerates exactly the table's rows on the given branch.
func TestRowPrefixScan(t *testing.T) {
	st := openTemp(t)
	ns := mustNS(t, "tenant-a")
	enc := st.Encoder()

	wb := st.NewWriteBatchAt(10)
	for _, pk := range []string{"a", "b", "c"} {
		if err := wb.Set(enc.RowKey(ns, "main", "users", []byte(pk)), []byte("v-"+pk)); err != nil {
			t.Fatal(err)
		}
	}
	// A row in a different table must NOT be matched by the users prefix.
	if err := wb.Set(enc.RowKey(ns, "main", "orders", []byte("o1")), []byte("order")); err != nil {
		t.Fatal(err)
	}
	if err := wb.Flush(); err != nil {
		t.Fatal(err)
	}

	rtx := st.NewTransactionAt(20)
	defer rtx.Discard()
	it := rtx.NewIterator(enc.RowPrefix(ns, "main", "users"))
	defer it.Close()

	count := 0
	for it.Rewind(); it.Valid(); it.Next() {
		count++
	}
	if count != 3 {
		t.Fatalf("expected 3 users rows under prefix, got %d", count)
	}
}

// TestSiblingMainVsBranch proves main (sibling /main) and a feature branch
// (/br/feature) occupy distinct key spaces (by design).
func TestSiblingMainVsBranch(t *testing.T) {
	st := openTemp(t)
	ns := mustNS(t, "tenant-a")
	enc := st.Encoder()

	mainKey := enc.RowKey(ns, "main", "users", []byte("pk-1"))
	brKey := enc.RowKey(ns, "feature-x", "users", []byte("pk-1"))

	if bytes.Equal(mainKey.Bytes(), brKey.Bytes()) {
		t.Fatal("main and branch row keys must differ")
	}
	if !bytes.Contains(mainKey.Bytes(), []byte("/main/")) {
		t.Fatalf("main key missing /main/ sibling segment: %q", mainKey.Bytes())
	}
	if !bytes.Contains(brKey.Bytes(), []byte("/br/feature-x/")) {
		t.Fatalf("branch key missing /br/feature-x/ segment: %q", brKey.Bytes())
	}
}

// TestNamespaceIsolationAndErasure proves DeleteNamespace removes only the
// target namespace's keys (GDPR erasure; namespace isolation).
func TestNamespaceIsolationAndErasure(t *testing.T) {
	st := openTemp(t)
	nsA := mustNS(t, "tenant-a")
	nsB := mustNS(t, "tenant-b")
	enc := st.Encoder()

	wb := st.NewWriteBatchAt(10)
	if err := wb.Set(enc.RowKey(nsA, "main", "t", []byte("1")), []byte("A")); err != nil {
		t.Fatal(err)
	}
	if err := wb.Set(enc.RowKey(nsB, "main", "t", []byte("1")), []byte("B")); err != nil {
		t.Fatal(err)
	}
	if err := wb.Flush(); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteNamespace(context.Background(), nsA); err != nil {
		t.Fatalf("delete namespace A: %v", err)
	}

	rtx := st.NewTransactionAt(20)
	defer rtx.Discard()

	if _, err := rtx.Get(enc.RowKey(nsA, "main", "t", []byte("1"))); err != ErrKeyNotFound {
		t.Fatalf("tenant-a key should be erased, got err=%v", err)
	}
	if got, err := rtx.Get(enc.RowKey(nsB, "main", "t", []byte("1"))); err != nil || !bytes.Equal(got, []byte("B")) {
		t.Fatalf("tenant-b key must survive erasure of A: got=%q err=%v", got, err)
	}
}

// TestIntentKeyColocated proves a Percolator intent key is co-located with its
// row (shares the row key as a prefix) — a design decision.
func TestIntentKeyColocated(t *testing.T) {
	enc := keyEncoder{}
	ns := mustNS(t, "tenant-a")
	row := enc.RowKey(ns, "main", "users", []byte("pk-1"))
	intent := enc.IntentKey(row, 42, IntentWrite)

	if !bytes.HasPrefix(intent.Bytes(), row.Bytes()) {
		t.Fatal("intent key must be co-located (row key is a prefix of the intent key)")
	}
	if intent.Bytes()[len(intent.Bytes())-1] != byte(IntentWrite) {
		t.Fatal("intent key must end with the intent suffix")
	}
}
