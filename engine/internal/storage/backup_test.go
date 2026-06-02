package storage

import (
	"bytes"
	"os"
	"testing"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
)

// bkTmpDir is a temp dir with best-effort cleanup: on Windows BadgerDB's mmap'd
// value log may linger briefly after Close, which would fail t.TempDir's strict
// RemoveAll. (No such issue on Linux CI.)
func bkTmpDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "bdb-bk-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func writeKey(t *testing.T, s Store, ns auth.NamespaceID, table, pk, val string, ts uint64) {
	t.Helper()
	wb := s.NewWriteBatchAt(ts)
	if err := wb.Set(s.Encoder().RowKey(ns, "main", table, []byte(pk)), []byte(val)); err != nil {
		t.Fatal(err)
	}
	if err := wb.Flush(); err != nil {
		t.Fatal(err)
	}
}

func readKey(t *testing.T, s Store, ns auth.NamespaceID, table, pk string, ts uint64) ([]byte, error) {
	t.Helper()
	rtx := s.NewTransactionAt(ts)
	defer rtx.Discard()
	return rtx.Get(s.Encoder().RowKey(ns, "main", table, []byte(pk)))
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	src, err := Open(Options{DataDir: bkTmpDir(t), ValueLogFileMB: 4})
	if err != nil {
		t.Fatal(err)
	}
	ns := mustNS(t, "tenant-a")
	writeKey(t, src, ns, "t", "k1", "hello", 1)
	writeKey(t, src, ns, "t", "k2", "world", 2)

	var buf bytes.Buffer
	if _, err := src.Backup(&buf); err != nil {
		t.Fatalf("backup: %v", err)
	}
	_ = src.Close()

	// Restore into a fresh store.
	dst, err := Open(Options{DataDir: bkTmpDir(t), ValueLogFileMB: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err := dst.Restore(&buf); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := readKey(t, dst, ns, "t", "k1", dst.MaxVersion())
	if err != nil {
		t.Fatalf("read after restore: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("restored k1 want hello, got %q", got)
	}
}

func TestEncryptionAtRestRoundTrip(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef") // 32-byte AES-256 key
	dir := bkTmpDir(t)
	ns := mustNS(t, "tenant-a")

	s1, err := Open(Options{DataDir: dir, EncryptionKey: key, ValueLogFileMB: 4})
	if err != nil {
		t.Fatalf("open encrypted: %v", err)
	}
	writeKey(t, s1, ns, "t", "secret", "classified", 1)
	_ = s1.Close()

	// Reopen with the same key: data is readable.
	s2, err := Open(Options{DataDir: dir, EncryptionKey: key, ValueLogFileMB: 4})
	if err != nil {
		t.Fatalf("reopen encrypted: %v", err)
	}
	defer s2.Close()
	got, err := readKey(t, s2, ns, "t", "secret", s2.MaxVersion())
	if err != nil || string(got) != "classified" {
		t.Fatalf("encrypted round-trip failed: got %q err %v", got, err)
	}
}
