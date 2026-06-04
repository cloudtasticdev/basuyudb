package auth

// Local role/password store for SCRAM-SHA-256 authentication.
//
// PERSISTENCE: roles created via CREATE/ALTER ROLE ... PASSWORD can now be
// durably persisted across restarts. Construct the store with
// NewPersistentRoleStore(path, encKey): mutations (UpsertPassword,
// UpsertVerifier, Delete) are flushed atomically to `path` (conventionally
// <dataDir>/roles.json). When an encryption key is supplied (the same
// BASUYUDB_ENCRYPTION_KEY used for BadgerDB), the file is encrypted with
// AES-256-GCM at rest; otherwise it is plain JSON. We store ONLY the SCRAM
// verifier (salt, iterations, StoredKey, ServerKey) — never the plaintext
// password — mirroring PostgreSQL's pg_authid. The plain NewRoleStore()
// constructor remains available for an in-memory-only store (used by tests and
// dev mode). See internal/auth/rolestore_persist.go for the on-disk format,
// atomic-write, and load-error behaviour.

import (
	"strings"
	"sync"
)

// RoleStore maps a (normalised) username to its SCRAM verifier. It is safe for
// concurrent use by multiple connection goroutines. When persist is non-nil,
// mutations are flushed to disk under the store lock.
type RoleStore struct {
	mu      sync.RWMutex
	roles   map[string]SCRAMVerifier
	persist *persistence // nil => in-memory only
}

// NewRoleStore constructs an empty in-memory role store (no persistence).
func NewRoleStore() *RoleStore {
	return &RoleStore{roles: make(map[string]SCRAMVerifier)}
}

// NewPersistentRoleStore constructs a role store backed by the file at path.
// Any existing roles are loaded at construction. If encKey is non-empty the
// file is encrypted at rest with AES-256-GCM (key derived to 32 bytes via
// SHA-256 when not already 32 bytes); otherwise it is plain JSON. A missing
// file yields an empty store; a corrupt or undecryptable file returns an error
// (we never silently discard existing credentials).
func NewPersistentRoleStore(path string, encKey []byte) (*RoleStore, error) {
	p := &persistence{path: path}
	if len(encKey) > 0 {
		p.encKey = append([]byte(nil), encKey...)
	}
	loaded, err := p.load()
	if err != nil {
		return nil, err
	}
	return &RoleStore{roles: loaded, persist: p}, nil
}

// saveLocked flushes the current map to disk if persistence is configured. The
// caller MUST hold s.mu (write lock).
func (s *RoleStore) saveLocked() error {
	if s.persist == nil {
		return nil
	}
	return s.persist.save(s.roles)
}

// normaliseRole canonicalises a role name for lookup. PostgreSQL role names are
// case-sensitive when quoted, but unquoted identifiers fold to lower case. The
// wire layer strips quotes before calling us; we trim spaces and keep the name
// as-is (callers pass the already-unquoted name). We do NOT lower-case so that
// quoted, case-sensitive role names round-trip correctly.
func normaliseRole(name string) string {
	return strings.TrimSpace(name)
}

// UpsertPassword derives a fresh SCRAM verifier from the plaintext password and
// stores it for username, replacing any existing entry. The plaintext is used
// only transiently to derive the verifier and is never retained.
func (s *RoleStore) UpsertPassword(username, password string) error {
	v, err := NewSCRAMVerifier(password)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roles[normaliseRole(username)] = v
	return s.saveLocked()
}

// UpsertVerifier stores a precomputed verifier for username (e.g. when loading
// from an external source). Replaces any existing entry and persists if backed
// by a file.
func (s *RoleStore) UpsertVerifier(username string, v SCRAMVerifier) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roles[normaliseRole(username)] = v
	return s.saveLocked()
}

// Lookup returns the stored verifier for username and whether it exists.
func (s *RoleStore) Lookup(username string) (SCRAMVerifier, bool) {
	s.mu.RLock()
	v, ok := s.roles[normaliseRole(username)]
	s.mu.RUnlock()
	return v, ok
}

// Has reports whether username has a locally provisioned password.
func (s *RoleStore) Has(username string) bool {
	_, ok := s.Lookup(username)
	return ok
}

// Delete removes a role from the store (DROP ROLE) and persists the change if
// backed by a file. No-op if absent.
func (s *RoleStore) Delete(username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.roles, normaliseRole(username))
	return s.saveLocked()
}
