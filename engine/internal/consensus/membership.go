package consensus

import (
	"context"
	"fmt"
)

// ReplicaID returns this node's replica id within the shard. Part of the
// structural surface consumed by the management server (mgmt.ClusterInfo).
func (n *Node) ReplicaID() uint64 { return n.cfg.ReplicaID }

// AppliedIndex returns the highest Raft index applied by the shard's state
// machine on this node — the managed-mode read timestamp. Part of the
// structural surface consumed by the management server (mgmt.ClusterInfo).
func (n *Node) AppliedIndex(shardID uint64) uint64 { return n.ReadTimestamp(shardID) }

// AddReplica adds a new voting replica to the shard's Raft membership. It does
// NOT start the replica's node — the new member must call StartShard with
// join=true to begin participating. configChangeIndex 0 lets dragonboat use the
// current membership config change index.
//
// dragonboat forwards the membership change to the leader, so this may be called
// from any node. The Sync* call needs a context deadline; the same default-guard
// the Propose methods use is applied.
func (n *Node) AddReplica(ctx context.Context, shardID, replicaID uint64, raftAddr string) error {
	ctx, cancel := withProposeDeadline(ctx)
	defer cancel()
	if err := n.nh.SyncRequestAddReplica(ctx, shardID, replicaID, raftAddr, 0); err != nil {
		return fmt.Errorf("consensus: add replica %d to shard %d: %w", replicaID, shardID, err)
	}
	return nil
}

// RemoveReplica removes a voting replica from the shard's Raft membership.
// configChangeIndex 0 lets dragonboat use the current config change index.
func (n *Node) RemoveReplica(ctx context.Context, shardID, replicaID uint64) error {
	ctx, cancel := withProposeDeadline(ctx)
	defer cancel()
	if err := n.nh.SyncRequestDeleteReplica(ctx, shardID, replicaID, 0); err != nil {
		return fmt.Errorf("consensus: remove replica %d from shard %d: %w", replicaID, shardID, err)
	}
	return nil
}

// Members returns the current voting members of the shard as replicaID->RaftAddress.
// It performs a linearizable read of the membership through the leader.
func (n *Node) Members(ctx context.Context, shardID uint64) (map[uint64]string, error) {
	ctx, cancel := withProposeDeadline(ctx)
	defer cancel()
	m, err := n.nh.SyncGetShardMembership(ctx, shardID)
	if err != nil {
		return nil, fmt.Errorf("consensus: get shard %d membership: %w", shardID, err)
	}
	out := make(map[uint64]string, len(m.Nodes))
	for rid, addr := range m.Nodes {
		out[rid] = addr
	}
	return out, nil
}
