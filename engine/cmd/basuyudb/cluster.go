package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cloudtasticdev/basuyudb/engine/internal/consensus"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// buildCommitter constructs the replication backend for the transaction engine.
//
//   - Single-node (default): returns (nil, no-op, nil). transactions.New falls
//     back to the LocalCommitter — identical behavior to pre-HA builds.
//   - Clustered (BASUYUDB_CLUSTER_ENABLED=true): starts a dragonboat Raft node,
//     joins/founds the shard backed by `store`, waits for a leader, and returns
//     the *consensus.Node (a ReplicatedCommitter) so Commit replicates through
//     the log. The returned stop() shuts the node down.
//
// Cluster env:
//
//	BASUYUDB_CLUSTER_ENABLED   "true" to enable Raft (default false → single node)
//	BASUYUDB_NODE_ID           replica id: a uint64, or a StatefulSet pod name
//	                           ("basuyudb-2" → ordinal 2 → replica id 3)
//	BASUYUDB_RAFT_ADDR         this node's Raft transport host:port (required)
//	BASUYUDB_RAFT_PEERS        founding membership "id@host:port,id@host:port,..."
//	                           (required unless joining)
//	BASUYUDB_RAFT_JOIN         "true" to join an existing cluster (default false)
//	BASUYUDB_RAFT_DIR          dragonboat NodeHost dir (default <dataDir>/raft)
//	BASUYUDB_RAFT_RTT_MS              network RTT estimate ms (default 50)
//	BASUYUDB_RAFT_ELECTION_TIMEOUT_MS  election timeout ms (default 500)
func buildCommitter(logger *slog.Logger, store storage.Store, dataDir string) (committer transactions.Committer, replicaID uint64, stop func(), err error) {
	noop := func() {}
	if !envBool("BASUYUDB_CLUSTER_ENABLED", false) {
		return nil, 1, noop, nil
	}

	replicaID, err = parseReplicaID(envStr("BASUYUDB_NODE_ID", "1"))
	if err != nil {
		return nil, 0, noop, fmt.Errorf("BASUYUDB_NODE_ID: %w", err)
	}
	raftAddr := envStr("BASUYUDB_RAFT_ADDR", "")
	if raftAddr == "" {
		return nil, 0, noop, fmt.Errorf("BASUYUDB_RAFT_ADDR is required when BASUYUDB_CLUSTER_ENABLED=true")
	}
	join := envBool("BASUYUDB_RAFT_JOIN", false)
	peers, err := parsePeers(envStr("BASUYUDB_RAFT_PEERS", ""))
	if err != nil {
		return nil, 0, noop, fmt.Errorf("BASUYUDB_RAFT_PEERS: %w", err)
	}
	if !join && len(peers) == 0 {
		return nil, 0, noop, fmt.Errorf("BASUYUDB_RAFT_PEERS is required to found a cluster (or set BASUYUDB_RAFT_JOIN=true)")
	}
	raftDir := envStr("BASUYUDB_RAFT_DIR", filepath.Join(dataDir, "raft"))

	node, err := consensus.New(consensus.Config{
		ReplicaID:      replicaID,
		RaftAddress:    raftAddr,
		DataDir:        raftDir,
		RTTMillisecond: uint64(envInt("BASUYUDB_RAFT_RTT_MS", 50)),
		ElectionRTT:    electionRTTFromTimeout(envInt("BASUYUDB_RAFT_ELECTION_TIMEOUT_MS", 500), envInt("BASUYUDB_RAFT_RTT_MS", 50)),
	})
	if err != nil {
		return nil, 0, noop, fmt.Errorf("start raft node: %w", err)
	}

	if err := node.StartShard(transactions.DefaultShardID, peers, join, store); err != nil {
		node.Stop()
		return nil, 0, noop, fmt.Errorf("start raft shard: %w", err)
	}

	// Block until the shard has a leader so the server is not advertised ready
	// while writes would fail. 30s covers a cold multi-node election.
	readyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := node.WaitReady(readyCtx, transactions.DefaultShardID); err != nil {
		node.Stop()
		return nil, 0, noop, fmt.Errorf("raft shard not ready: %w", err)
	}

	leader, _ := node.LeaderID(transactions.DefaultShardID)
	logger.Info("raft cluster ready",
		"replica_id", replicaID, "raft_addr", raftAddr, "members", len(peers),
		"join", join, "leader_replica", leader)

	return node, replicaID, node.Stop, nil
}

// parseReplicaID accepts a bare uint64 or a StatefulSet pod name with a trailing
// ordinal ("basuyudb-2"). Pod ordinals are 0-based; replica ids are 1-based, so
// ordinal 2 → replica id 3.
func parseReplicaID(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	if v, err := strconv.ParseUint(s, 10, 64); err == nil {
		if v == 0 {
			return 0, fmt.Errorf("replica id must be >= 1")
		}
		return v, nil
	}
	m := trailingOrdinal.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("not a uint64 or pod-name-with-ordinal: %q", s)
	}
	ord, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0, err
	}
	return ord + 1, nil
}

var trailingOrdinal = regexp.MustCompile(`-(\d+)$`)

// parsePeers parses "id@host:port,id@host:port,..." into a replicaID→address map.
func parsePeers(s string) (map[uint64]string, error) {
	out := map[uint64]string{}
	s = strings.TrimSpace(s)
	if s == "" {
		return out, nil
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		at := strings.IndexByte(part, '@')
		if at <= 0 || at == len(part)-1 {
			return nil, fmt.Errorf("bad peer %q (want id@host:port)", part)
		}
		id, err := strconv.ParseUint(part[:at], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("bad peer id in %q: %w", part, err)
		}
		out[id] = part[at+1:]
	}
	return out, nil
}

// electionRTTFromTimeout converts an election timeout in ms to dragonboat RTT
// units (timeout / rtt), clamped to a sane minimum of 5.
func electionRTTFromTimeout(timeoutMs, rttMs int) uint64 {
	if rttMs <= 0 {
		rttMs = 50
	}
	rtt := timeoutMs / rttMs
	if rtt < 5 {
		rtt = 5
	}
	return uint64(rtt)
}
