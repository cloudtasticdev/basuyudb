package consensus

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lni/dragonboat/v4"
	"github.com/lni/dragonboat/v4/config"
	sm "github.com/lni/dragonboat/v4/statemachine"

	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// Config configures a consensus Node.
type Config struct {
	ReplicaID uint64 // this node's replica id within the shard
	RaftAddress string // host:port for inter-node Raft transport
	DataDir string // dragonboat NodeHost dir (Raft log + snapshots)
	RTTMillisecond uint64 // network round-trip estimate (default 50)
	ElectionRTT uint64 // election timeout in RTT units (default 10 → 500ms)
	HeartbeatRTT uint64 // heartbeat interval in RTT units (default 1)
}

// Node wraps a dragonboat NodeHost and the per-shard state machines. It
// implements transactions.Committer: Propose replicates a committed write batch
// through Raft and returns once applied on a quorum (by design).
type Node struct {
	nh *dragonboat.NodeHost
	cfg Config
	mu sync.Mutex
	machines map[uint64]*stateMachine // shardID -> SM (for read timestamps)
	stopOnce sync.Once
}

var _ transactions.Committer = (*Node)(nil)

// New starts a dragonboat NodeHost.
func New(cfg Config) (*Node, error) {
	if cfg.RTTMillisecond == 0 {
		cfg.RTTMillisecond = 50
	}
	if cfg.ElectionRTT == 0 {
		cfg.ElectionRTT = 10 // 10 * 50ms = 500ms (ADR-018 election_timeout_ms=500)
	}
	if cfg.HeartbeatRTT == 0 {
		cfg.HeartbeatRTT = 1
	}
	nhc := config.NodeHostConfig{
		WALDir: cfg.DataDir,
		NodeHostDir: cfg.DataDir,
		RTTMillisecond: cfg.RTTMillisecond,
		RaftAddress: cfg.RaftAddress,
	}
	nh, err := dragonboat.NewNodeHost(nhc)
	if err != nil {
		return nil, fmt.Errorf("consensus: new node host: %w", err)
	}
	return &Node{nh: nh, cfg: cfg, machines: map[uint64]*stateMachine{}}, nil
}

// StartShard starts (or joins) a Raft shard backed by the given store. The
// state machine applies replicated batches to the store. initialMembers maps
// replicaID -> RaftAddress for the founding members (ignored when join=true).
func (n *Node) StartShard(shardID uint64, initialMembers map[uint64]string, join bool, store storage.Store) error {
	base := newStateMachineFunc(store)
	create := func(sid, rid uint64) sm.IStateMachine {
		machine := base(sid, rid).(*stateMachine)
		n.mu.Lock()
		n.machines[sid] = machine
		n.mu.Unlock()
		return machine
	}
	cfg := config.Config{
		ReplicaID: n.cfg.ReplicaID,
		ShardID: shardID,
		ElectionRTT: n.cfg.ElectionRTT,
		HeartbeatRTT: n.cfg.HeartbeatRTT,
		CheckQuorum: true,
		SnapshotEntries: 0, // milestone: disable auto-snapshot (store is durable)
		CompactionOverhead: 0,
	}
	members := make(map[uint64]dragonboat.Target, len(initialMembers))
	for rid, addr := range initialMembers {
		members[rid] = addr
	}
	if err := n.nh.StartReplica(members, join, create, cfg); err != nil {
		return fmt.Errorf("consensus: start shard %d: %w", shardID, err)
	}
	return nil
}

// Propose replicates a committed write batch through Raft. It implements
// transactions.Committer (by design): returns after the batch
// is applied by the state machine on a quorum.
func (n *Node) Propose(ctx context.Context, shardID uint64, batch []transactions.Mutation) error {
	if len(batch) == 0 {
		return nil
	}
	cs := n.nh.GetNoOPSession(shardID)
	_, err := n.nh.SyncPropose(ctx, cs, marshalBatch(batch))
	if err != nil {
		return fmt.Errorf("consensus: propose to shard %d: %w", shardID, err)
	}
	return nil
}

// ReadTimestamp returns a safe managed-mode read timestamp for a shard (the
// highest Raft index applied by its state machine).
func (n *Node) ReadTimestamp(shardID uint64) uint64 {
	n.mu.Lock()
	machine := n.machines[shardID]
	n.mu.Unlock()
	if machine == nil {
		return 0
	}
	return machine.LastApplied()
}

// WaitReady blocks until the shard has a leader or the timeout elapses.
func (n *Node) WaitReady(ctx context.Context, shardID uint64) error {
	for {
		if _, _, ok, err := n.nh.GetLeaderID(shardID); err == nil && ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("consensus: shard %d not ready: %w", shardID, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// LeaderID returns the current leader replica id for a shard.
func (n *Node) LeaderID(shardID uint64) (uint64, bool) {
	id, _, ok, err := n.nh.GetLeaderID(shardID)
	if err != nil {
		return 0, false
	}
	return id, ok
}

// Stop shuts down the NodeHost (idempotent).
func (n *Node) Stop() { n.stopOnce.Do(func() { n.nh.Close() }) }

// WaitNewLeader blocks until a leader other than `notThis` is elected (used to
// observe failover after a leader is stopped).
func (n *Node) WaitNewLeader(ctx context.Context, shardID, notThis uint64) (uint64, error) {
	for {
		if id, _, ok, err := n.nh.GetLeaderID(shardID); err == nil && ok && id != notThis {
			return id, nil
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("consensus: no new leader for shard %d: %w", shardID, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
