// Package consensus wires BasuyuDB writes through a dragonboat (Apache 2.0)
// multi-group Raft so committed mutations replicate across the cluster
// (ADR-007, Gate 4). A replicated state machine deterministically applies each
// proposed write batch to the managed BadgerDB store at the entry's Raft log
// index (used as the managed-mode commit timestamp). consensus.NodeHost
// implements transactions.Committer (by design): the same
// transactions.Commit path that runs locally on a single node replicates
// through Raft once a NodeHost is wired.
package consensus

import (
	"encoding/binary"
	"fmt"

	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// noConflictReadTS marks a proposal that skips the snapshot conflict check (the
// direct Propose path / tests). math.MaxUint64 means "no committed version can
// be newer than my read snapshot", so the state machine always applies it.
const noConflictReadTS = ^uint64(0)

// marshalBatch serialises a committed mutation batch for direct proposal (no
// snapshot conflict check). Equivalent to marshalProposal(noConflictReadTS, …).
func marshalBatch(muts []transactions.Mutation) []byte {
	return marshalProposal(noConflictReadTS, muts)
}

// marshalProposal serialises a transaction proposal through Raft. Layout:
// uint64 readTS (the proposer's snapshot, for the state machine's deterministic
// first-committer-wins check), uint32 count, then per mutation: byte
// delete-flag, uint32 keyLen, key, uint32 valLen, val.
func marshalProposal(readTS uint64, muts []transactions.Mutation) []byte {
	size := 8 + 4
	for _, m := range muts {
		size += 1 + 4 + len(m.Key.Bytes()) + 4 + len(m.Value)
	}
	out := make([]byte, 0, size)
	out = binary.BigEndian.AppendUint64(out, readTS)
	out = binary.BigEndian.AppendUint32(out, uint32(len(muts)))
	var lp [4]byte
	for _, m := range muts {
		if m.Delete {
			out = append(out, 1)
		} else {
			out = append(out, 0)
		}
		kb := m.Key.Bytes()
		binary.BigEndian.PutUint32(lp[:], uint32(len(kb)))
		out = append(out, lp[:]...)
		out = append(out, kb...)
		binary.BigEndian.PutUint32(lp[:], uint32(len(m.Value)))
		out = append(out, lp[:]...)
		out = append(out, m.Value...)
	}
	return out
}

// appliedMutation is a decoded write to apply to the store. The key is rebuilt
// via storage.RawKeyForMerge (a Key from already-encoded bytes — the bytes came
// from a KeyEncoder on the proposing node).
type appliedMutation struct {
	key storage.Key
	value []byte
	delete bool
}

// unmarshalProposal decodes a marshalProposal payload into the proposer's read
// snapshot and the mutations to apply.
func unmarshalProposal(b []byte) (readTS uint64, muts []appliedMutation, err error) {
	if len(b) < 12 {
		return 0, nil, fmt.Errorf("consensus: short proposal")
	}
	readTS = binary.BigEndian.Uint64(b[:8])
	n := binary.BigEndian.Uint32(b[8:12])
	pos := 12
	out := make([]appliedMutation, 0, n)
	for i := uint32(0); i < n; i++ {
		if pos+1+4 > len(b) {
			return 0, nil, fmt.Errorf("consensus: truncated mutation header")
		}
		del := b[pos] == 1
		pos++
		klen := int(binary.BigEndian.Uint32(b[pos:]))
		pos += 4
		if pos+klen+4 > len(b) {
			return 0, nil, fmt.Errorf("consensus: truncated key")
		}
		key := append([]byte(nil), b[pos:pos+klen]...)
		pos += klen
		vlen := int(binary.BigEndian.Uint32(b[pos:]))
		pos += 4
		if pos+vlen > len(b) {
			return 0, nil, fmt.Errorf("consensus: truncated value")
		}
		val := append([]byte(nil), b[pos:pos+vlen]...)
		pos += vlen
		out = append(out, appliedMutation{key: storage.RawKeyForMerge(key), value: val, delete: del})
	}
	return readTS, out, nil
}
