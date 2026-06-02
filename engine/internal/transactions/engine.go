package transactions

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
)

// Mutation is a single key write or delete within a transaction.
type Mutation struct {
	Key storage.Key
	Value []byte // nil for a delete (tombstone)
	Delete bool
}

// Committer is the narrow consensus dependency a commit needs to replicate. On
// a single node this is the LocalCommitter (no replication). At Gate 4
// consensus.NodeHost implements it via Propose. (by design)
type Committer interface {
	// Propose submits a committed write batch for the namespace/shard. It
	// returns after the batch is durably applied (locally on a single node;
	// on a quorum once Raft is wired).
	Propose(ctx context.Context, shardID uint64, batch []Mutation) error
}

// LocalCommitter is the single-node Committer: it is a no-op because Commit has
// already applied the batch to the managed store. It exists so the call site is
// identical to the Gate-4 Raft path.
type LocalCommitter struct{}

func (LocalCommitter) Propose(context.Context, uint64, []Mutation) error { return nil }

// Txn is an in-flight transaction.
type Txn struct {
	ReadTS HLCTimestamp
	readUint uint64
	sess auth.Session
	mutations []Mutation
	done bool
}

// ReadTimestamp returns the managed-mode uint64 read timestamp for this txn.
func (t *Txn) ReadTimestamp() uint64 { return t.readUint }

// Session returns the owning auth session.
func (t *Txn) Session() auth.Session { return t.sess }

// TransactionEngine is the canonical Percolator coordinator surface. The
// single-node engine implements the subset required for milestone-3; the
// CheckTxnStatus/TxnHeartbeat lazy-cleanup methods arrive with the distributed
// path at Gate 4.
type TransactionEngine interface {
	Begin(ctx context.Context, sess auth.Session) (*Txn, error)
	Get(ctx context.Context, txn *Txn, k storage.Key) ([]byte, error)
	NewIterator(txn *Txn, prefix storage.Key) storage.Iterator
	NewReverseIterator(txn *Txn, prefix storage.Key) storage.Iterator
	Buffer(txn *Txn, m Mutation)
	Commit(ctx context.Context, txn *Txn) error
	Rollback(ctx context.Context, txn *Txn) error
	HLC() *HLC
}

// ErrTxnDone is returned when a committed/rolled-back txn is reused.
var ErrTxnDone = errors.New("transactions: transaction already completed")

// engine is the single-node TransactionEngine over a managed Store.
type engine struct {
	store storage.Store
	hlc *HLC
	committer Committer
	// clock is the managed-mode timestamp oracle. It is strictly monotonic and
	// drives BadgerDB read/commit timestamps. Reads see all commits with a
	// commit timestamp <= the reader's snapshot.
	clock atomic.Uint64
}

// New constructs the single-node transaction engine. nodeID seeds the HLC.
func New(store storage.Store, nodeID uint64, committer Committer) TransactionEngine {
	if committer == nil {
		committer = LocalCommitter{}
	}
	e := &engine{store: store, hlc: NewHLC(nodeID), committer: committer}
	// Resume the timestamp oracle from the highest committed version persisted in
	// the store. On a fresh store MaxVersion is 0 → start at 1 (ts 0 reserved).
	// On reopen this ensures a read snapshot (clock.Load) sees all previously
	// committed data and the next commit (clock.Add(1)) is strictly newer.
	start := store.MaxVersion()
	if start < 1 {
		start = 1
	}
	e.clock.Store(start)
	return e
}

func (e *engine) HLC() *HLC { return e.hlc }

func (e *engine) Begin(ctx context.Context, sess auth.Session) (*Txn, error) {
	ts := e.hlc.Now()
	return &Txn{
		ReadTS: ts,
		readUint: e.clock.Load(), // snapshot: see everything committed so far
		sess: sess,
	}, nil
}

// Get reads a key at the txn's snapshot, honouring this txn's own buffered
// writes (read-your-writes).
func (e *engine) Get(ctx context.Context, txn *Txn, k storage.Key) ([]byte, error) {
	if txn.done {
		return nil, ErrTxnDone
	}
	// Read-your-writes: scan buffered mutations newest-first.
	for i := len(txn.mutations) - 1; i >= 0; i-- {
		m := txn.mutations[i]
		if string(m.Key.Bytes()) == string(k.Bytes()) {
			if m.Delete {
				return nil, storage.ErrKeyNotFound
			}
			return m.Value, nil
		}
	}
	rtx := e.store.NewTransactionAt(txn.readUint)
	defer rtx.Discard()
	return rtx.Get(k)
}

// NewIterator returns a snapshot iterator at the txn's read timestamp. (Buffered
// writes are not merged into the scan for milestone-3; callers that need
// read-your-writes during a scan are not yet present. Branch fallthrough is
// added when engine/internal/branch lands.)
func (e *engine) NewIterator(txn *Txn, prefix storage.Key) storage.Iterator {
	rtx := e.store.NewTransactionAt(txn.readUint)
	return &snapshotIterator{Iterator: rtx.NewIterator(prefix), txn: rtx}
}

// NewReverseIterator mirrors NewIterator but scans the prefix in descending key
// order (ORDER BY ... DESC over an ordered index). Buffered writes are not
// merged, identical to NewIterator.
func (e *engine) NewReverseIterator(txn *Txn, prefix storage.Key) storage.Iterator {
	rtx := e.store.NewTransactionAt(txn.readUint)
	return &snapshotIterator{Iterator: rtx.NewReverseIterator(prefix), txn: rtx}
}

// snapshotIterator owns the read transaction for the iterator's lifetime.
type snapshotIterator struct {
	storage.Iterator
	txn storage.Txn
}

func (s *snapshotIterator) Close() {
	s.Iterator.Close()
	s.txn.Discard()
}

func (e *engine) Buffer(txn *Txn, m Mutation) {
	txn.mutations = append(txn.mutations, m)
}

// Commit applies all buffered mutations atomically at a fresh commit timestamp
// via a managed WriteBatch, then notifies the Committer (no-op on a single
// node; Raft Propose at Gate 4). The clock is advanced only after a durable
// flush, so a reader that began before the commit never observes a partial
// write (snapshot isolation).
func (e *engine) Commit(ctx context.Context, txn *Txn) error {
	if txn.done {
		return ErrTxnDone
	}
	txn.done = true
	if len(txn.mutations) == 0 {
		return nil
	}

	commitTS := e.clock.Add(1)
	wb := e.store.NewWriteBatchAt(commitTS)
	for _, m := range txn.mutations {
		var err error
		if m.Delete {
			err = wb.Delete(m.Key)
		} else {
			err = wb.Set(m.Key, m.Value)
		}
		if err != nil {
			wb.Cancel()
			return err
		}
	}
	if err := wb.Flush(); err != nil {
		return err
	}
	// Replicate (single-node: no-op). The edge is identical to the Gate-4 path.
	return e.committer.Propose(ctx, 0, txn.mutations)
}

func (e *engine) Rollback(ctx context.Context, txn *Txn) error {
	if txn.done {
		return ErrTxnDone
	}
	txn.done = true
	txn.mutations = nil
	return nil
}
