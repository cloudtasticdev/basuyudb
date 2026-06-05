// Package mgmt provides a small stdlib-only HTTP management surface for a
// clustered BasuyuDB node. It exposes a Patroni-style REST API so HAProxy,
// Kubernetes probes, and operators can discover the primary, route reads to
// replicas, and mutate cluster membership.
//
// The server depends only on the minimal ClusterInfo interface so it can be
// unit-tested without a real Raft node. The concrete *consensus.Node satisfies
// ClusterInfo structurally — mgmt deliberately does NOT import consensus, so
// there is no import cycle.
package mgmt

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// shard is the single Raft shard the engine uses.
const shard = transactions.DefaultShardID

// ClusterInfo is the minimal view of a Raft node the management server needs.
// *consensus.Node satisfies this structurally.
type ClusterInfo interface {
	ReplicaID() uint64
	LeaderID(shard uint64) (uint64, bool)
	AppliedIndex(shard uint64) uint64
	Members(ctx context.Context, shard uint64) (map[uint64]string, error)
	AddReplica(ctx context.Context, shard, replicaID uint64, addr string) error
	RemoveReplica(ctx context.Context, shard, replicaID uint64) error
}

// Server is the management HTTP server. Construct with NewServer.
type Server struct {
	addr   string
	info   ClusterInfo
	logger *slog.Logger
	// adminToken, when non-empty, gates the membership-mutating endpoints
	// (/members/add, /members/remove). Read endpoints are always unauthenticated
	// (HAProxy/k8s probe them like Patroni).
	adminToken string

	srv *http.Server
	ln  net.Listener
}

// Option configures a Server.
type Option func(*Server)

// WithAdminToken gates the membership-mutating endpoints behind a bearer token
// compared in constant time. When the token is empty, mutations are still
// served (and logged) but unauthenticated — pass a non-empty token in
// production to require authorization.
func WithAdminToken(token string) Option {
	return func(s *Server) { s.adminToken = token }
}

// NewServer builds a management server bound to addr (e.g. ":8080") backed by
// info. It does not listen until Start is called.
func NewServer(addr string, info ClusterInfo, logger *slog.Logger, opts ...Option) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{addr: addr, info: info, logger: logger}
	for _, opt := range opts {
		opt(s)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/leader", s.handleLeader)
	mux.HandleFunc("/readonly", s.handleReadonly)
	mux.HandleFunc("/replica", s.handleReadonly) // alias
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/members/add", s.handleMembersAdd)
	mux.HandleFunc("/members/remove", s.handleMembersRemove)
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// Handler exposes the underlying mux handler (useful for httptest).
func (s *Server) Handler() http.Handler { return s.srv.Handler }

// Start binds the listener and serves in a background goroutine (non-blocking).
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.ln = ln
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Warn("management server stopped", "err", err)
		}
	}()
	s.logger.Info("management server listening", "addr", ln.Addr().String())
	return nil
}

// Close shuts the server down gracefully.
func (s *Server) Close() error {
	if s.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

func (s *Server) isLeader() (leaderID uint64, ok bool, isLeader bool) {
	leaderID, ok = s.info.LeaderID(shard)
	return leaderID, ok, ok && leaderID == s.info.ReplicaID()
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleLeader returns 200 only when THIS node is the current Raft leader, so
// HAProxy routes writes to the primary (Patroni-style).
func (s *Server) handleLeader(w http.ResponseWriter, r *http.Request) {
	if _, _, leader := s.isLeader(); leader {
		writeJSON(w, http.StatusOK, map[string]string{"role": "leader"})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"role": "follower"})
}

// handleReadonly returns 200 when this node is a follower (read replica).
func (s *Server) handleReadonly(w http.ResponseWriter, r *http.Request) {
	_, ok, leader := s.isLeader()
	if ok && !leader {
		writeJSON(w, http.StatusOK, map[string]string{"role": "replica"})
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"role": "leader"})
}

type statusResponse struct {
	ReplicaID    uint64            `json:"replica_id"`
	LeaderID     uint64            `json:"leader_id"`
	IsLeader     bool              `json:"is_leader"`
	AppliedIndex uint64            `json:"applied_index"`
	Members      map[uint64]string `json:"members"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	leaderID, _, leader := s.isLeader()
	resp := statusResponse{
		ReplicaID:    s.info.ReplicaID(),
		LeaderID:     leaderID,
		IsLeader:     leader,
		AppliedIndex: s.info.AppliedIndex(shard),
	}
	members, err := s.info.Members(r.Context(), shard)
	if err != nil {
		// Membership read can fail (no leader yet); still report node-local state.
		resp.Members = map[uint64]string{}
		s.logger.Debug("status: members read failed", "err", err)
	} else {
		resp.Members = members
	}
	writeJSON(w, http.StatusOK, resp)
}

type memberAddRequest struct {
	ReplicaID uint64 `json:"replica_id"`
	Addr      string `json:"addr"`
}

func (s *Server) handleMembersAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req memberAddRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.ReplicaID == 0 || req.Addr == "" {
		writeError(w, http.StatusBadRequest, "replica_id and addr are required")
		return
	}
	s.logger.Info("membership mutation: add replica", "replica_id", req.ReplicaID, "addr", req.Addr)
	if err := s.info.AddReplica(r.Context(), shard, req.ReplicaID, req.Addr); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "replica_id": req.ReplicaID})
}

type memberRemoveRequest struct {
	ReplicaID uint64 `json:"replica_id"`
}

func (s *Server) handleMembersRemove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req memberRemoveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.ReplicaID == 0 {
		writeError(w, http.StatusBadRequest, "replica_id is required")
		return
	}
	s.logger.Info("membership mutation: remove replica", "replica_id", req.ReplicaID)
	if err := s.info.RemoveReplica(r.Context(), shard, req.ReplicaID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "replica_id": req.ReplicaID})
}

// authorized reports whether the request may mutate membership. When no admin
// token is configured, mutations are allowed (and logged). When a token is set,
// it must match the Authorization: Bearer <token> header in constant time.
func (s *Server) authorized(r *http.Request) bool {
	if s.adminToken == "" {
		return true
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return false
	}
	got := h[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.adminToken)) == 1
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
