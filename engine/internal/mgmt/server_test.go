package mgmt

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeClusterInfo is a configurable in-memory ClusterInfo for unit tests.
type fakeClusterInfo struct {
	mu        sync.Mutex
	replicaID uint64
	leaderID  uint64
	hasLeader bool
	applied   uint64
	members   map[uint64]string
	membersErr error

	addCalls    []addCall
	removeCalls []uint64
}

type addCall struct {
	replicaID uint64
	addr      string
}

func (f *fakeClusterInfo) ReplicaID() uint64 { return f.replicaID }

func (f *fakeClusterInfo) LeaderID(uint64) (uint64, bool) { return f.leaderID, f.hasLeader }

func (f *fakeClusterInfo) AppliedIndex(uint64) uint64 { return f.applied }

func (f *fakeClusterInfo) Members(context.Context, uint64) (map[uint64]string, error) {
	if f.membersErr != nil {
		return nil, f.membersErr
	}
	return f.members, nil
}

func (f *fakeClusterInfo) AddReplica(_ context.Context, _, replicaID uint64, addr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addCalls = append(f.addCalls, addCall{replicaID, addr})
	return nil
}

func (f *fakeClusterInfo) RemoveReplica(_ context.Context, _, replicaID uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeCalls = append(f.removeCalls, replicaID)
	return nil
}

func newTestServer(info ClusterInfo, opts ...Option) *Server {
	return NewServer(":0", info, nil, opts...)
}

func doReq(t *testing.T, s *Server, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, path, bytes.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestHealthAlwaysOK(t *testing.T) {
	s := newTestServer(&fakeClusterInfo{})
	w := doReq(t, s, http.MethodGet, "/health", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("health: got %d want 200", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("health body: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("health status: got %q want ok", body["status"])
	}
}

func TestLeaderEndpoint(t *testing.T) {
	leader := &fakeClusterInfo{replicaID: 2, leaderID: 2, hasLeader: true}
	if w := doReq(t, newTestServer(leader), http.MethodGet, "/leader", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("leader node /leader: got %d want 200", w.Code)
	}

	follower := &fakeClusterInfo{replicaID: 3, leaderID: 2, hasLeader: true}
	if w := doReq(t, newTestServer(follower), http.MethodGet, "/leader", nil, nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("follower node /leader: got %d want 503", w.Code)
	}
}

func TestReadonlyEndpoint(t *testing.T) {
	follower := &fakeClusterInfo{replicaID: 3, leaderID: 2, hasLeader: true}
	for _, path := range []string{"/readonly", "/replica"} {
		if w := doReq(t, newTestServer(follower), http.MethodGet, path, nil, nil); w.Code != http.StatusOK {
			t.Fatalf("follower %s: got %d want 200", path, w.Code)
		}
	}

	leader := &fakeClusterInfo{replicaID: 2, leaderID: 2, hasLeader: true}
	if w := doReq(t, newTestServer(leader), http.MethodGet, "/readonly", nil, nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("leader /readonly: got %d want 503", w.Code)
	}
}

func TestStatusShape(t *testing.T) {
	info := &fakeClusterInfo{
		replicaID: 2, leaderID: 2, hasLeader: true, applied: 42,
		members: map[uint64]string{1: "a:1", 2: "b:2", 3: "c:3"},
	}
	w := doReq(t, newTestServer(info), http.MethodGet, "/status", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", w.Code)
	}
	var resp statusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("status decode: %v", err)
	}
	if resp.ReplicaID != 2 || resp.LeaderID != 2 || !resp.IsLeader || resp.AppliedIndex != 42 {
		t.Fatalf("status fields wrong: %+v", resp)
	}
	if len(resp.Members) != 3 || resp.Members[1] != "a:1" {
		t.Fatalf("status members wrong: %+v", resp.Members)
	}
}

func TestMembersAddCallsThrough(t *testing.T) {
	info := &fakeClusterInfo{replicaID: 1, leaderID: 1, hasLeader: true}
	s := newTestServer(info)
	body, _ := json.Marshal(memberAddRequest{ReplicaID: 4, Addr: "host:9000"})
	w := doReq(t, s, http.MethodPost, "/members/add", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("members/add: got %d want 200 (body %s)", w.Code, w.Body.String())
	}
	if len(info.addCalls) != 1 || info.addCalls[0].replicaID != 4 || info.addCalls[0].addr != "host:9000" {
		t.Fatalf("AddReplica not called as expected: %+v", info.addCalls)
	}
}

func TestMembersRemoveCallsThrough(t *testing.T) {
	info := &fakeClusterInfo{replicaID: 1, leaderID: 1, hasLeader: true}
	s := newTestServer(info)
	body, _ := json.Marshal(memberRemoveRequest{ReplicaID: 3})
	w := doReq(t, s, http.MethodPost, "/members/remove", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("members/remove: got %d want 200 (body %s)", w.Code, w.Body.String())
	}
	if len(info.removeCalls) != 1 || info.removeCalls[0] != 3 {
		t.Fatalf("RemoveReplica not called as expected: %+v", info.removeCalls)
	}
}

func TestMembersAddRequiresToken(t *testing.T) {
	info := &fakeClusterInfo{replicaID: 1, leaderID: 1, hasLeader: true}
	s := newTestServer(info, WithAdminToken("s3cret"))

	body, _ := json.Marshal(memberAddRequest{ReplicaID: 4, Addr: "host:9000"})

	// No token → 401.
	if w := doReq(t, s, http.MethodPost, "/members/add", body, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d want 401", w.Code)
	}
	if len(info.addCalls) != 0 {
		t.Fatalf("AddReplica should not have been called without token")
	}

	// Correct token → 200.
	w := doReq(t, s, http.MethodPost, "/members/add", body, map[string]string{"Authorization": "Bearer s3cret"})
	if w.Code != http.StatusOK {
		t.Fatalf("with token: got %d want 200 (body %s)", w.Code, w.Body.String())
	}
	if len(info.addCalls) != 1 {
		t.Fatalf("AddReplica should have been called once with valid token")
	}
}

func TestMembersAddRejectsGET(t *testing.T) {
	s := newTestServer(&fakeClusterInfo{})
	if w := doReq(t, s, http.MethodGet, "/members/add", nil, nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET members/add: got %d want 405", w.Code)
	}
}
