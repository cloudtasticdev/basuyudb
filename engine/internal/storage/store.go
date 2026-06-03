package storage

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	badger "github.com/dgraph-io/badger/v4"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
)

// ErrKeyNotFound is returned by Txn.Get / Store reads when a key is absent.
var ErrKeyNotFound = badger.ErrKeyNotFound

// Txn is a managed-mode transaction handle. Read transactions are opened at an
// explicit read timestamp; writes are committed at an explicit commit
// timestamp. This is the managed-mode contract (by design).
type Txn interface {
	Get(k Key) ([]byte, error) // returns ErrKeyNotFound if absent
	Set(k Key, val []byte) error
	Delete(k Key) error
	NewIterator(prefix Key) Iterator
	// NewReverseIterator scans keys under prefix in descending order. Rewind
	// positions at the largest key carrying the prefix; Next moves to smaller
	// keys. Used for ORDER BY ... DESC over an ordered index.
	NewReverseIterator(prefix Key) Iterator
	Discard()
	// CommitAt commits all writes at commitTs (managed mode). Read-only txns
	// must call Discard instead.
	CommitAt(commitTs uint64) error
}

// Iterator scans keys in a managed-mode transaction.
type Iterator interface {
	Rewind()
	// Seek positions the iterator at k (forward: first key >= k; reverse: first
	// key <= k), bounded by the iterator's prefix. Used to start a range scan at
	// a computed lower/upper bound.
	Seek(k Key)
	Valid() bool
	Next()
	Key() Key
	Value() ([]byte, error)
	Close()
}

// WriteBatch is a managed-mode batched writer committing at a fixed commitTs.
type WriteBatch interface {
	Set(k Key, val []byte) error
	Delete(k Key) error
	Flush() error
	Cancel()
}

// Store is the ONLY IO path. It wraps the managed-mode BadgerDB instance.
// (by design)
type Store interface {
	Encoder() KeyEncoder
	NewTransactionAt(readTs uint64) Txn
	NewWriteBatchAt(commitTs uint64) WriteBatch
	SetDiscardTs(ts uint64)
	// MaxVersion returns the maximum committed managed-mode timestamp persisted
	// in the store. On reopen it is the basis for resuming the timestamp oracle
	// so reads see all previously committed data — without it a restarted oracle
	// would read below the persisted commit timestamps and miss everything.
	MaxVersion() uint64
	// LatestVersion returns the commit timestamp of a key's newest committed
	// version (found=false if the key has never been written). It backs the
	// transaction layer's first-committer-wins write-conflict detection.
	LatestVersion(k Key) (ts uint64, found bool, err error)
	Sync() error
	// Backup streams a consistent BadgerDB backup of all committed data to w and
	// returns the version backed up. Restore loads a backup stream into this
	// (empty) store. They are the engine's point-in-time backup primitive.
	Backup(w io.Writer) (uint64, error)
	Restore(r io.Reader) error
	// BadgerDB is the documented escape hatch returning the underlying *badger.DB
	// as interface{}. It exists ONLY for HNSW (008) and FTS (009). (by design)
	BadgerDB() interface{}
	DeleteNamespace(ctx context.Context, ns auth.NamespaceID) error
	Close() error
}

// Options configures a Store. Sizes follow the design specs §10.
type Options struct {
	DataDir string // BASUYUDB_DATA_DIR (e.g. /data/badger)
	MemTableSizeMB int64 // default 64
	ValueThreshold int64 // default 4096
	BlockCacheMB int64 // default max(1024, dataDir/10); 0 => 1024
	ValueLogFileMB int64 // default 128 (vlog files rotate; caps preallocation)
	EncryptionKey []byte // BASUYUDB_ENCRYPTION_KEY (optional, instance-level)
	Logger *slog.Logger
	VlogGCInterval time.Duration // default 5m (G-ADR-26)
	VlogGCRatio float64 // default 0.5
}

func (o *Options) withDefaults() {
	if o.MemTableSizeMB == 0 {
		o.MemTableSizeMB = 64
	}
	if o.ValueThreshold == 0 {
		o.ValueThreshold = 4096
	}
	if o.BlockCacheMB == 0 {
		o.BlockCacheMB = 1024
	}
	if o.ValueLogFileMB == 0 {
		o.ValueLogFileMB = 128
	}
	if o.VlogGCInterval == 0 {
		o.VlogGCInterval = 5 * time.Minute
	}
	if o.VlogGCRatio == 0 {
		o.VlogGCRatio = 0.5
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// badgerStore is the managed-mode BadgerDB-backed Store.
type badgerStore struct {
	db *badger.DB
	enc keyEncoder
	logger *slog.Logger
	gcStop chan struct{}
	gcDone chan struct{}
	closeMu sync.Once
}

// Open opens the single managed-mode BadgerDB data instance. It is the ONLY
// badger.Open call for the data instance. (by design)
func Open(o Options) (Store, error) {
	o.withDefaults()

	bopts := badger.DefaultOptions(o.DataDir).
		WithMemTableSize(o.MemTableSizeMB << 20).
		WithValueThreshold(o.ValueThreshold).
		WithBlockCacheSize(o.BlockCacheMB << 20).
		WithValueLogFileSize(o.ValueLogFileMB << 20).
		WithLogger(badgerSlogAdapter{o.Logger}).
		WithCompactL0OnClose(true)

	if len(o.EncryptionKey) > 0 {
		bopts = bopts.WithEncryptionKey(o.EncryptionKey).WithIndexCacheSize(128 << 20)
	}

	// MANAGED MODE: OpenManaged enforces managedTxns=true. Reads use explicit
	// read timestamps; writes commit at explicit commit timestamps. This is a
	// one-way door inherited by every consumer. (by design)
	db, err := badger.OpenManaged(bopts)
	if err != nil {
		return nil, fmt.Errorf("storage: open managed badger at %q: %w", o.DataDir, err)
	}

	s := &badgerStore{
		db: db,
		logger: o.Logger,
		gcStop: make(chan struct{}),
		gcDone: make(chan struct{}),
	}
	go s.vlogGCLoop(o.VlogGCInterval, o.VlogGCRatio)
	o.Logger.Info("storage: managed BadgerDB opened",
		"data_dir", o.DataDir,
		"encrypted", len(o.EncryptionKey) > 0,
		"managed_mode", true,
	)
	return s, nil
}

func (s *badgerStore) Encoder() KeyEncoder { return s.enc }

func (s *badgerStore) NewTransactionAt(readTs uint64) Txn {
	// update=true so the executor can read-modify-write within the snapshot;
	// pure readers simply Discard.
	return &badgerTxn{txn: s.db.NewTransactionAt(readTs, true)}
}

func (s *badgerStore) NewWriteBatchAt(commitTs uint64) WriteBatch {
	return &badgerWriteBatch{wb: s.db.NewWriteBatchAt(commitTs)}
}

func (s *badgerStore) SetDiscardTs(ts uint64) { s.db.SetDiscardTs(ts) }

func (s *badgerStore) MaxVersion() uint64 { return s.db.MaxVersion() }

// LatestVersion reads a key's newest committed version timestamp at MaxVersion.
func (s *badgerStore) LatestVersion(k Key) (uint64, bool, error) {
	txn := s.db.NewTransactionAt(s.db.MaxVersion(), false)
	defer txn.Discard()
	item, err := txn.Get(k.Bytes())
	if errors.Is(err, badger.ErrKeyNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return item.Version(), true, nil
}

func (s *badgerStore) Sync() error { return s.db.Sync() }

// Backup writes a managed-mode-safe backup to w: every live key/value at the
// current max version, each as a length-prefixed chunk (4-byte big-endian
// length + bytes), key then value. BadgerDB's own DB.Backup uses NewStream
// which panics under managed transactions, so we stream the snapshot directly.
func (s *badgerStore) Backup(w io.Writer) (uint64, error) {
	ver := s.db.MaxVersion()
	txn := s.db.NewTransactionAt(ver, false)
	defer txn.Discard()
	it := txn.NewIterator(badger.DefaultIteratorOptions)
	defer it.Close()
	for it.Rewind(); it.Valid(); it.Next() {
		item := it.Item()
		k := item.KeyCopy(nil)
		v, err := item.ValueCopy(nil)
		if err != nil {
			return 0, err
		}
		if err := writeChunk(w, k); err != nil {
			return 0, err
		}
		if err := writeChunk(w, v); err != nil {
			return 0, err
		}
	}
	return ver, nil
}

// Restore loads a backup produced by Backup into this (empty) store, applying
// all pairs at a single commit version.
func (s *badgerStore) Restore(r io.Reader) error {
	wb := s.db.NewWriteBatchAt(1)
	for {
		k, err := readChunk(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			wb.Cancel()
			return err
		}
		v, err := readChunk(r)
		if err != nil {
			wb.Cancel()
			return err
		}
		if err := wb.SetEntryAt(badger.NewEntry(k, v), 1); err != nil {
			wb.Cancel()
			return err
		}
	}
	return wb.Flush()
}

func writeChunk(w io.Writer, b []byte) error {
	var lp [4]byte
	binary.BigEndian.PutUint32(lp[:], uint32(len(b)))
	if _, err := w.Write(lp[:]); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

func readChunk(r io.Reader) ([]byte, error) {
	var lp [4]byte
	if _, err := io.ReadFull(r, lp[:]); err != nil {
		return nil, err // io.EOF at a clean boundary signals end
	}
	n := binary.BigEndian.Uint32(lp[:])
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

func (s *badgerStore) BadgerDB() interface{} { return s.db }

// DeleteNamespace erases the /ns/{ns}/ prefix atomically (GDPR). (by design)
func (s *badgerStore) DeleteNamespace(ctx context.Context, ns auth.NamespaceID) error {
	if ns.IsZero() {
		return errors.New("storage: DeleteNamespace requires a validated namespace")
	}
	prefix := s.enc.NamespacePrefix(ns).Bytes()
	if err := s.db.DropPrefix(prefix); err != nil {
		return fmt.Errorf("storage: delete namespace %q: %w", ns.String(), err)
	}
	return nil
}

func (s *badgerStore) Close() error {
	var err error
	s.closeMu.Do(func() {
		close(s.gcStop)
		<-s.gcDone // wait for GC goroutine to stop before closing db
		err = s.db.Close()
	})
	return err
}

// vlogGCLoop runs value-log GC on a ticker (G-ADR-26 operational requirement).
func (s *badgerStore) vlogGCLoop(interval time.Duration, ratio float64) {
	defer close(s.gcDone)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.gcStop:
			return
		case <-t.C:
			// RunValueLogGC returns ErrNoRewrite when nothing to reclaim.
			for s.db.RunValueLogGC(ratio) == nil {
				// keep collecting until no more rewrites
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Txn / Iterator / WriteBatch wrappers
// ---------------------------------------------------------------------------

type badgerTxn struct {
	txn *badger.Txn
}

func (t *badgerTxn) Get(k Key) ([]byte, error) {
	item, err := t.txn.Get(k.Bytes())
	if err != nil {
		return nil, err // badger.ErrKeyNotFound passes through (== ErrKeyNotFound)
	}
	return item.ValueCopy(nil)
}

func (t *badgerTxn) Set(k Key, val []byte) error { return t.txn.Set(k.Bytes(), val) }
func (t *badgerTxn) Delete(k Key) error { return t.txn.Delete(k.Bytes()) }
func (t *badgerTxn) Discard() { t.txn.Discard() }
func (t *badgerTxn) CommitAt(commitTs uint64) error { return t.txn.CommitAt(commitTs, nil) }

func (t *badgerTxn) NewIterator(prefix Key) Iterator {
	opts := badger.DefaultIteratorOptions
	opts.PrefetchValues = false
	it := t.txn.NewIterator(opts)
	return &badgerIterator{it: it, prefix: prefix.Bytes()}
}

func (t *badgerTxn) NewReverseIterator(prefix Key) Iterator {
	opts := badger.DefaultIteratorOptions
	opts.PrefetchValues = false
	opts.Reverse = true
	it := t.txn.NewIterator(opts)
	return &badgerIterator{it: it, prefix: prefix.Bytes(), reverse: true}
}

type badgerIterator struct {
	it *badger.Iterator
	prefix []byte
	reverse bool
}

// Rewind positions at the start of the scan: the smallest key with the prefix
// (forward), or the largest (reverse). For reverse, seek to the prefix's
// successor — Badger then lands on the next-smaller key, i.e. the largest key
// still carrying the prefix.
func (i *badgerIterator) Rewind() {
	if i.reverse {
		i.it.Seek(prefixSuccessor(i.prefix))
		return
	}
	i.it.Seek(i.prefix)
}
func (i *badgerIterator) Seek(k Key) { i.it.Seek(k.Bytes()) }
func (i *badgerIterator) Valid() bool { return i.it.ValidForPrefix(i.prefix) }
func (i *badgerIterator) Next() { i.it.Next() }
func (i *badgerIterator) Key() Key { return rawKey(i.it.Item().KeyCopy(nil)) }
func (i *badgerIterator) Value() ([]byte, error) {
	return i.it.Item().ValueCopy(nil)
}
func (i *badgerIterator) Close() { i.it.Close() }

// prefixSuccessor returns the smallest byte string strictly greater than every
// string having prefix p — p with its last non-0xFF byte incremented and the
// trailing 0xFF bytes dropped. Returns nil if p is all 0xFF (no successor).
func prefixSuccessor(p []byte) []byte {
	out := append([]byte(nil), p...)
	for len(out) > 0 {
		if out[len(out)-1] != 0xFF {
			out[len(out)-1]++
			return out
		}
		out = out[:len(out)-1]
	}
	return nil
}

type badgerWriteBatch struct {
	wb *badger.WriteBatch
}

func (w *badgerWriteBatch) Set(k Key, val []byte) error { return w.wb.Set(k.Bytes(), val) }
func (w *badgerWriteBatch) Delete(k Key) error { return w.wb.Delete(k.Bytes()) }
func (w *badgerWriteBatch) Flush() error { return w.wb.Flush() }
func (w *badgerWriteBatch) Cancel() { w.wb.Cancel() }

// ---------------------------------------------------------------------------
// badger logger adapter (routes badger's internal logs through slog)
// ---------------------------------------------------------------------------

type badgerSlogAdapter struct{ l *slog.Logger }

func (a badgerSlogAdapter) Errorf(f string, v ...interface{}) { a.l.Error(fmt.Sprintf(f, v...)) }
func (a badgerSlogAdapter) Warningf(f string, v ...interface{}) { a.l.Warn(fmt.Sprintf(f, v...)) }
func (a badgerSlogAdapter) Infof(f string, v ...interface{}) { a.l.Info(fmt.Sprintf(f, v...)) }
func (a badgerSlogAdapter) Debugf(f string, v ...interface{}) { a.l.Debug(fmt.Sprintf(f, v...)) }
