package consensus

import (
	"encoding/binary"
	"io"
	"sync/atomic"

	sm "github.com/lni/dragonboat/v4/statemachine"

	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
)

// stateMachine is the replicated state machine. On every replica it
// deterministically applies a proposed write batch to the managed BadgerDB
// store at the entry's Raft log Index, used as the managed-mode commit
// timestamp. Because the same (index, batch) is applied identically on every
// replica, the stores converge.
type stateMachine struct {
	store storage.Store
	shardID uint64
	replicaID uint64
	lastApplied atomic.Uint64
}

// newStateMachine returns a dragonboat CreateStateMachineFunc bound to a store.
func newStateMachineFunc(store storage.Store) func(shardID, replicaID uint64) sm.IStateMachine {
	return func(shardID, replicaID uint64) sm.IStateMachine {
		return &stateMachine{store: store, shardID: shardID, replicaID: replicaID}
	}
}

// Update applies one committed entry. The Raft log Index is the managed-mode
// commit timestamp, guaranteeing monotonic, deterministic application.
func (s *stateMachine) Update(e sm.Entry) (sm.Result, error) {
	muts, err := unmarshalBatch(e.Cmd)
	if err != nil {
		return sm.Result{}, err
	}
	wb := s.store.NewWriteBatchAt(e.Index)
	for _, m := range muts {
		if m.delete {
			if err := wb.Delete(m.key); err != nil {
				wb.Cancel()
				return sm.Result{}, err
			}
		} else {
			if err := wb.Set(m.key, m.value); err != nil {
				wb.Cancel()
				return sm.Result{}, err
			}
		}
	}
	if err := wb.Flush(); err != nil {
		return sm.Result{}, err
	}
	s.lastApplied.Store(e.Index)
	return sm.Result{Value: uint64(len(muts))}, nil
}

// LastApplied returns the highest Raft index applied (the safe read timestamp).
func (s *stateMachine) LastApplied() uint64 { return s.lastApplied.Load() }

// Lookup reports the last-applied index (used for read-your-writes after a
// proposal in tests).
func (s *stateMachine) Lookup(interface{}) (interface{}, error) {
	return s.lastApplied.Load(), nil
}

// SaveSnapshot writes the last-applied index. The managed BadgerDB store is
// durable independently of the Raft snapshot; on recovery the log is replayed
// and re-applied idempotently (same index → same commit timestamp).
func (s *stateMachine) SaveSnapshot(w io.Writer, _ sm.ISnapshotFileCollection, _ <-chan struct{}) error {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], s.lastApplied.Load())
	_, err := w.Write(b[:])
	return err
}

// RecoverFromSnapshot restores the last-applied marker.
func (s *stateMachine) RecoverFromSnapshot(r io.Reader, _ []sm.SnapshotFile, _ <-chan struct{}) error {
	var b [8]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return err
	}
	s.lastApplied.Store(binary.BigEndian.Uint64(b[:]))
	return nil
}

// Close releases the state machine (the store is owned by the engine).
func (s *stateMachine) Close() error { return nil }
