package consensus

import (
	"context"
	"testing"
	"time"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

func ns(t *testing.T) auth.NamespaceID {
	t.Helper()
	s, err := auth.SessionFromClaims(&auth.SessionClaims{
		Sub: "u", Jti: "j", Role: "service",
		NamespaceAccess: []string{"tenant-a"}, NamespaceID: "tenant-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s.Namespace
}

// TestRaftProposeApplies forms a single-replica Raft shard, proposes a committed
// write batch through Raft, and verifies the replicated state machine applied it
// to the managed BadgerDB store. This proves the Gate-4 commit→Propose→apply
// edge (by design): the same path replicates across a quorum
// in a multi-node cluster.
func TestRaftProposeApplies(t *testing.T) {
	store, err := storage.Open(storage.Options{DataDir: t.TempDir(), ValueLogFileMB: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	node, err := New(Config{
		ReplicaID: 1,
		RaftAddress: "127.0.0.1:63010",
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Stop()

	const shard = 1
	if err := node.StartShard(shard, map[uint64]string{1: "127.0.0.1:63010"}, false, store); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := node.WaitReady(ctx, shard); err != nil {
		t.Fatalf("shard never elected a leader: %v", err)
	}

	// The node should have elected itself leader (Gate 4: cluster forms).
	if id, ok := node.LeaderID(shard); !ok || id != 1 {
		t.Fatalf("expected replica 1 as leader, got id=%d ok=%v", id, ok)
	}

	// Propose a committed batch through Raft (transactions.Committer path).
	enc := store.Encoder()
	k1 := enc.RowKey(ns(t), "main", "users", []byte("u1"))
	k2 := enc.RowKey(ns(t), "main", "users", []byte("u2"))
	batch := []transactions.Mutation{
		{Key: k1, Value: []byte("alice")},
		{Key: k2, Value: []byte("bob")},
	}
	if err := node.Propose(ctx, shard, batch); err != nil {
		t.Fatalf("propose: %v", err)
	}

	// Read the store at a timestamp >= the applied Raft index: the replicated
	// writes are present.
	readTS := node.ReadTimestamp(shard) + 1
	rtx := store.NewTransactionAt(readTS)
	defer rtx.Discard()
	if v, err := rtx.Get(k1); err != nil || string(v) != "alice" {
		t.Fatalf("replicated k1 want alice, got %q err=%v", v, err)
	}
	if v, err := rtx.Get(k2); err != nil || string(v) != "bob" {
		t.Fatalf("replicated k2 want bob, got %q err=%v", v, err)
	}

	// A delete proposed through Raft applies too.
	if err := node.Propose(ctx, shard, []transactions.Mutation{{Key: k1, Delete: true}}); err != nil {
		t.Fatalf("propose delete: %v", err)
	}
	rtx2 := store.NewTransactionAt(node.ReadTimestamp(shard) + 1)
	defer rtx2.Discard()
	if _, err := rtx2.Get(k1); err != storage.ErrKeyNotFound {
		t.Fatalf("k1 should be deleted via Raft, err=%v", err)
	}
}
