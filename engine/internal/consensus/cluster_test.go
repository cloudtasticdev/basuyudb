package consensus

import (
	"context"
	"testing"
	"time"

	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// TestThreeNodeReplicationAndFailover forms a 3-replica Raft shard in one
// process (each replica on its own port with its own store), replicates a write
// to a quorum, kills the leader, verifies a new leader is elected, and confirms
// writes continue — the full Gate-4 acceptance: cluster forms, replicates,
// survives leader failure.
func TestThreeNodeReplicationAndFailover(t *testing.T) {
	const shard = 1
	addrs := map[uint64]string{
		1: "127.0.0.1:63021",
		2: "127.0.0.1:63022",
		3: "127.0.0.1:63023",
	}

	type member struct {
		node *Node
		store storage.Store
	}
	members := map[uint64]*member{}

	for rid := uint64(1); rid <= 3; rid++ {
		st, err := storage.Open(storage.Options{DataDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		n, err := New(Config{ReplicaID: rid, RaftAddress: addrs[rid], DataDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		members[rid] = &member{node: n, store: st}
		t.Cleanup(func() { n.Stop(); st.Close() })
	}

	// Start all three replicas with the same initial membership.
	for rid, m := range members {
		if err := m.node.StartShard(shard, addrs, false, m.store); err != nil {
			t.Fatalf("replica %d start: %v", rid, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Wait for a leader on replica 1's view.
	if err := members[1].node.WaitReady(ctx, shard); err != nil {
		t.Fatalf("cluster never formed: %v", err)
	}
	leader, ok := members[1].node.LeaderID(shard)
	if !ok {
		t.Fatal("no leader after formation")
	}
	t.Logf("cluster formed; leader = replica %d", leader)

	// Propose a write through any node (followers forward to the leader).
	enc := members[1].store.Encoder()
	key := enc.RowKey(ns(t), "main", "t", []byte("k1"))
	if err := members[1].node.Propose(ctx, shard, []transactions.Mutation{{Key: key, Value: []byte("v1")}}); err != nil {
		t.Fatalf("propose: %v", err)
	}

	// The write must converge on a quorum of stores. Followers apply
	// asynchronously after the leader commits, so poll briefly. Read at a high
	// managed-mode timestamp so any applied entry is visible.
	const highTS = uint64(1) << 40
	quorum := func() int {
		n := 0
		for _, m := range members {
			rtx := m.store.NewTransactionAt(highTS)
			if v, err := rtx.Get(key); err == nil && string(v) == "v1" {
				n++
			}
			rtx.Discard()
		}
		return n
	}
	deadline := time.Now().Add(10 * time.Second)
	replicated := 0
	for time.Now().Before(deadline) {
		if replicated = quorum(); replicated >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if replicated < 2 {
		t.Fatalf("write replicated to only %d/3 stores (want quorum)", replicated)
	}
	t.Logf("write converged on %d/3 stores", replicated)

	// Kill the leader and confirm a new leader is elected.
	t.Logf("killing leader replica %d", leader)
	members[leader].node.Stop()

	var survivor *Node
	for rid, m := range members {
		if rid != leader {
			survivor = m.node
			break
		}
	}
	failCtx, failCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer failCancel()
	newLeader, err := survivor.WaitNewLeader(failCtx, shard, leader)
	if err != nil {
		t.Fatalf("no new leader after failure: %v", err)
	}
	t.Logf("survived failover; new leader = replica %d", newLeader)

	// Writes continue on the surviving quorum.
	key2 := enc.RowKey(ns(t), "main", "t", []byte("k2"))
	if err := survivor.Propose(failCtx, shard, []transactions.Mutation{{Key: key2, Value: []byte("v2")}}); err != nil {
		t.Fatalf("post-failover propose: %v", err)
	}
}
