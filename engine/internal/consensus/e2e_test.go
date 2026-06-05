package consensus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
	"github.com/cloudtasticdev/basuyudb/engine/internal/executor"
	"github.com/cloudtasticdev/basuyudb/engine/internal/parser"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// haSession builds a service session in the test namespace, shared by every
// node's executor so replicated row keys line up.
func haSession(t *testing.T) *session.Session {
	t.Helper()
	s, err := auth.SessionFromClaims(&auth.SessionClaims{
		Sub: "u", Jti: "j", Role: "service",
		NamespaceAccess: []string{ns(t).String()}, NamespaceID: ns(t).String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return session.New(s, 1, nil)
}

type haNode struct {
	rid   uint64
	node  *Node
	store storage.Store
	exec  executor.Executor
}

func mustSQL(t *testing.T, ex executor.Executor, sess *session.Session, sql string) {
	t.Helper()
	stmt, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	if _, err := ex.Execute(context.Background(), stmt, sess, nil); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// waitScalar polls a single-column/single-row query on one node's executor until
// it returns want (followers apply replicated entries asynchronously).
func waitScalar(t *testing.T, hn *haNode, sess *session.Session, sql, want string, timeout time.Duration) {
	t.Helper()
	stmt, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		res, err := hn.exec.Execute(context.Background(), stmt, sess, nil)
		if err == nil && len(res.Rows) == 1 && len(res.Rows[0]) == 1 {
			last = res.Rows[0][0].Text
			if last == want {
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("replica %d: %q never returned %q (last=%q)", hn.rid, sql, want, last)
}

// TestEndToEndClusterSQLFailover proves the FULL stack is HA: SQL DDL/DML runs
// through the executor → transaction engine → Raft → replicated state machine →
// store on a 3-node cluster; writes replicate to followers; the leader is killed
// and a new one is elected; writes resume on the survivor and all data — old and
// new — is intact and queryable. This is what makes external HA tooling (Patroni)
// unnecessary: failover and replication are built in.
func TestEndToEndClusterSQLFailover(t *testing.T) {
	const shard = transactions.DefaultShardID
	addrs := map[uint64]string{1: freeAddr(t), 2: freeAddr(t), 3: freeAddr(t)}
	nodes := map[uint64]*haNode{}
	for rid := uint64(1); rid <= 3; rid++ {
		st, err := storage.Open(storage.Options{DataDir: t.TempDir(), ValueLogFileMB: 4})
		if err != nil {
			t.Fatal(err)
		}
		n, err := New(Config{ReplicaID: rid, RaftAddress: addrs[rid], DataDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		hn := &haNode{rid: rid, node: n, store: st}
		nodes[rid] = hn
		t.Cleanup(func() { n.Stop(); st.Close() })
	}
	for rid, hn := range nodes {
		if err := hn.node.StartShard(shard, addrs, false, hn.store); err != nil {
			t.Fatalf("replica %d start: %v", rid, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := nodes[1].node.WaitReady(ctx, shard); err != nil {
		t.Fatalf("cluster never formed: %v", err)
	}

	// Each node gets a Raft-backed executor (committer = its own consensus node).
	for _, hn := range nodes {
		te := transactions.New(hn.store, hn.rid, hn.node)
		hn.exec = executor.New(hn.store, te)
	}
	sess := haSession(t)

	leaderRID, ok := nodes[1].node.LeaderID(shard)
	if !ok {
		t.Fatal("no leader after formation")
	}
	t.Logf("cluster formed; leader = replica %d", leaderRID)
	leader := nodes[leaderRID]

	// DDL + DML through the leader's executor — replicated through Raft.
	mustSQL(t, leader.exec, sess, "CREATE TABLE kv (id int PRIMARY KEY, v text)")
	mustSQL(t, leader.exec, sess, "INSERT INTO kv (id, v) VALUES (1, 'one'), (2, 'two')")

	// Read-your-writes on the leader (its SyncPropose applied locally before
	// returning), and replication convergence on every node.
	for _, hn := range nodes {
		waitScalar(t, hn, sess, "SELECT v FROM kv WHERE id = 1", "one", 15*time.Second)
		waitScalar(t, hn, sess, "SELECT v FROM kv WHERE id = 2", "two", 15*time.Second)
	}
	t.Logf("write replicated to all 3 nodes")

	// Kill the leader; a new leader must be elected from the surviving quorum.
	t.Logf("killing leader replica %d", leaderRID)
	leader.node.Stop()
	var surv *haNode
	for rid, hn := range nodes {
		if rid != leaderRID {
			surv = hn
			break
		}
	}
	failCtx, failCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer failCancel()
	newLeader, err := surv.node.WaitNewLeader(failCtx, shard, leaderRID)
	if err != nil {
		t.Fatalf("no new leader after failover: %v", err)
	}
	t.Logf("survived failover; new leader = replica %d", newLeader)

	// Writes resume on the surviving quorum, with read-your-writes, and the
	// pre-failover data is intact.
	mustSQL(t, surv.exec, sess, "INSERT INTO kv (id, v) VALUES (3, 'three')")
	waitScalar(t, surv, sess, "SELECT v FROM kv WHERE id = 3", "three", 15*time.Second)
	waitScalar(t, surv, sess, "SELECT v FROM kv WHERE id = 1", "one", 15*time.Second)
	t.Logf("post-failover write committed and historical data intact")
}

// TestClusterWriteConflict proves first-committer-wins snapshot isolation is
// enforced by the replicated state machine (deterministically, on apply): two
// transactions that read the same snapshot and write the same key — the first
// commits, the second is aborted with ErrWriteConflict (SQLSTATE 40001).
func TestClusterWriteConflict(t *testing.T) {
	const shard = transactions.DefaultShardID
	addr := freeAddr(t)
	st, err := storage.Open(storage.Options{DataDir: t.TempDir(), ValueLogFileMB: 4})
	if err != nil {
		t.Fatal(err)
	}
	node, err := New(Config{ReplicaID: 1, RaftAddress: addr, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { node.Stop(); st.Close() })
	if err := node.StartShard(shard, map[uint64]string{1: addr}, false, st); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := node.WaitReady(ctx, shard); err != nil {
		t.Fatal(err)
	}

	te := transactions.New(st, 1, node)
	sess := haSession(t)
	key := st.Encoder().RowKey(sess.Namespace(), "main", "t", []byte("k"))

	// Two transactions at the same snapshot, both writing the same key.
	tx1, err := te.Begin(ctx, sess.Auth)
	if err != nil {
		t.Fatal(err)
	}
	tx2, err := te.Begin(ctx, sess.Auth)
	if err != nil {
		t.Fatal(err)
	}
	te.Buffer(tx1, transactions.Mutation{Key: key, Value: []byte("a")})
	te.Buffer(tx2, transactions.Mutation{Key: key, Value: []byte("b")})

	if err := te.Commit(ctx, tx1); err != nil {
		t.Fatalf("first commit should win: %v", err)
	}
	err = te.Commit(ctx, tx2)
	if !errors.Is(err, transactions.ErrWriteConflict) {
		t.Fatalf("second commit should conflict, got: %v", err)
	}
	t.Logf("first-committer-wins enforced by the replicated state machine")
}
