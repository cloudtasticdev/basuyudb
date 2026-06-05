package transactions

import (
	"context"
	"errors"
	"sync"
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

// DefaultShardID is the single Raft group that backs the whole keyspace in the
// current single-shard cluster topology. (Multi-shard sharding is future work.)
const DefaultShardID uint64 = 1

// ReplicatedCommitter is the consensus dependency for a clustered (Raft) engine.
// When the committer passed to New implements it, Commit replicates writes
// THROUGH the log (the replicated state machine performs the conflict check and
// applies at the Raft index) instead of flushing locally, and reads snapshot at
// the state machine's applied index. consensus.Node implements it.
type ReplicatedCommitter interface {
	Committer
	// ProposeTxn replicates a transaction's batch and returns committed=false
	// (no error) when it lost a first-committer-wins race. index is the commit
	// timestamp (Raft log index) on success.
	ProposeTxn(ctx context.Context, shardID, readTS uint64, batch []Mutation) (committed bool, index uint64, err error)
	// ReadTimestamp returns the highest commit timestamp (Raft index) applied
	// locally — the safe snapshot for a new read.
	ReadTimestamp(shardID uint64) uint64
}

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

// ErrWriteConflict is returned by Commit when another transaction committed a
// newer version of a key in this transaction's write set since its read
// snapshot — first-committer-wins snapshot isolation. Callers retry (autocommit
// statements retry internally; explicit transactions surface SQLSTATE 40001).
var ErrWriteConflict = errors.New("transactions: write-write conflict")

// engine is the single-node TransactionEngine over a managed Store.
type engine struct {
	store storage.Store
	hlc *HLC
	committer Committer
	// repl is non-nil when the committer is a Raft (ReplicatedCommitter). In that
	// mode Commit proposes through the log and reads snapshot at the replicated
	// state machine's applied index, rather than flushing locally with the local
	// oracle. shardID is the Raft group serving this engine's keyspace.
	repl ReplicatedCommitter
	shardID uint64
	// clock is the managed-mode timestamp oracle. It is strictly monotonic and
	// drives BadgerDB read/commit timestamps. Reads see all commits with a
	// commit timestamp <= the reader's snapshot.
	clock atomic.Uint64
	// commitMu serializes the conflict-check + flush so the check and the commit
	// timestamp assignment are atomic (no commit can interleave between them).
	commitMu sync.Mutex
}

// New constructs the single-node transaction engine. nodeID seeds the HLC.
func New(store storage.Store, nodeID uint64, committer Committer) TransactionEngine {
	if committer == nil {
		committer = LocalCommitter{}
	}
	e := &engine{store: store, hlc: NewHLC(nodeID), committer: committer, shardID: DefaultShardID}
	if r, ok := committer.(ReplicatedCommitter); ok {
		e.repl = r
	}
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
	readUint := e.clock.Load() // single-node: see everything committed so far
	if e.repl != nil {
		// Clustered: snapshot at the replicated state machine's applied index.
		readUint = e.repl.ReadTimestamp(e.shardID)
	}
	return &Txn{
		ReadTS: ts,
		readUint: readUint,
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
	base := &snapshotIterator{Iterator: rtx.NewIterator(prefix), txn: rtx}
	// Overlay the transaction's own buffered writes (read-your-writes for scans).
	if buf := bufferedForPrefix(txn.mutations, prefix.Bytes()); len(buf) > 0 {
		return &mergedIterator{store: base, buf: buf}
	}
	return base
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
	if len(txn.mutations) == 0 {
		txn.done = true
		return nil
	}

	// Clustered path: replicate THROUGH the Raft log. The replicated state
	// machine performs the deterministic first-committer-wins conflict check and
	// applies at the Raft index on every node (no local flush here). SyncPropose
	// returns after this node has applied, giving read-your-writes.
	if e.repl != nil {
		txn.done = true
		committed, _, err := e.repl.ProposeTxn(ctx, e.shardID, txn.readUint, txn.mutations)
		if err != nil {
			return err
		}
		if !committed {
			return ErrWriteConflict
		}
		return nil
	}

	// First-committer-wins: serialize commits, then reject if any written key has
	// a committed version newer than this txn's read snapshot.
	e.commitMu.Lock()
	defer e.commitMu.Unlock()
	for _, m := range txn.mutations {
		ts, found, err := e.store.LatestVersion(m.Key)
		if err != nil {
			return err
		}
		if found && ts > txn.readUint {
			txn.done = true
			return ErrWriteConflict
		}
	}
	txn.done = true

	// Reserve a commit timestamp but DO NOT publish it to the read oracle until
	// the batch is durably flushed — otherwise a reader could capture this
	// timestamp as its snapshot and observe a half-applied commit (read skew).
	commitTS := e.clock.Load() + 1
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
	// Now durable: publish the commit timestamp to the read oracle so subsequent
	// snapshots see this commit atomically (all keys, never a subset).
	e.clock.Store(commitTS)
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
