// Package transactions is the BasuyuDB transaction engine. For V0.1 milestone-3
// it provides a single-node, snapshot-isolated transaction manager over the
// managed-mode storage.Store: reads at an explicit read timestamp, writes
// committed at an explicit commit timestamp. The interface and the co-located
// intent model follow the design specs §6 (Percolator) so that Gate 4 can
// extend Commit to propose through consensus (the Committer seam) without
// changing callers.
//
// Single-node scope: Commit applies the buffered mutations locally through a
// managed WriteBatch and notifies the Committer (a no-op locally). At Gate 4
// the Committer becomes consensus.NodeHost.Propose and the same Commit path
// replicates through Raft. (by design)
package transactions

import (
	"sync"
	"time"
)

// HLCTimestamp is the hybrid logical clock value. NodeID is uint64 end-to-end
// (unified with consensus/membership node IDs — the architecture review/the architecture review). (CANONICAL §6.)
type HLCTimestamp struct {
	WallNanos uint64
	Logical uint32
	NodeID uint64
}

// Encode packs the HLC into a monotonic uint64 suitable for a managed-mode
// BadgerDB timestamp and for intent-key ordering. The top 48 bits carry
// millisecond wall time; the low 16 bits carry the logical counter. This keeps
// timestamps monotonic and comparable across a single node; the full nanos +
// nodeID are retained in the struct for the distributed (Gate 4) path.
func (t HLCTimestamp) Encode() uint64 {
	ms := t.WallNanos / 1_000_000
	return (ms << 16) | uint64(t.Logical&0xFFFF)
}

// HLC is a hybrid logical clock. It guarantees strictly monotonic timestamps
// even when the wall clock does not advance between calls.
type HLC struct {
	mu sync.Mutex
	lastWall uint64
	logical uint32
	nodeID uint64
	now func() uint64 // injectable for tests; defaults to wall clock
}

// NewHLC constructs an HLC for the given node.
func NewHLC(nodeID uint64) *HLC {
	return &HLC{nodeID: nodeID, now: func() uint64 { return uint64(time.Now().UnixNano()) }}
}

// Now returns the next monotonic HLC timestamp.
func (h *HLC) Now() HLCTimestamp {
	h.mu.Lock()
	defer h.mu.Unlock()
	wall := h.now()
	if wall > h.lastWall {
		h.lastWall = wall
		h.logical = 0
	} else {
		// wall clock did not advance: bump the logical counter to stay monotonic.
		h.logical++
	}
	return HLCTimestamp{WallNanos: h.lastWall, Logical: h.logical, NodeID: h.nodeID}
}

// Update advances the clock past a timestamp observed from another node (used on
// the distributed path at Gate 4). Safe to call on a single node (no-op-ish).
func (h *HLC) Update(remote HLCTimestamp) HLCTimestamp {
	h.mu.Lock()
	defer h.mu.Unlock()
	wall := h.now()
	maxWall := wall
	if remote.WallNanos > maxWall {
		maxWall = remote.WallNanos
	}
	switch {
	case maxWall == h.lastWall:
		h.logical++
	case maxWall == remote.WallNanos:
		if remote.Logical >= h.logical {
			h.logical = remote.Logical + 1
		} else {
			h.logical++
		}
		h.lastWall = maxWall
	default:
		h.lastWall = maxWall
		h.logical = 0
	}
	return HLCTimestamp{WallNanos: h.lastWall, Logical: h.logical, NodeID: h.nodeID}
}
