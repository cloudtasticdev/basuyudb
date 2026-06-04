// Package session is a NEUTRAL package that breaks the wire<->executor import
// cycle. It carries the validated auth.Session plus per-connection request
// state. executor imports session; executor does NOT import wire. wire builds a
// *session.Session and passes it to executor.Execute.
// (by design)
package session

import (
	"fmt"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
)

// Session is threaded wire -> executor -> storage. It derives its identity from
// auth.Session: it carries the validated RAW namespace string (NOT a hashed
// nsID). the design specs's old keyenc.SessionContext is DELETED and replaced by
// reads off this type. (by design)
type Session struct {
	Auth auth.Session // the validated capability token
	connID uint64
	params map[string]string // PG startup parameters (incl. "branch")
	overrideBranch string // set via `SET branch = '...'` mid-session
	// settings holds per-session run-time configuration parameters set via
	// SET / set_config() — both standard GUCs and custom namespaced ones such as
	// app.current_tenant. Keys are lowercased because PostgreSQL GUC names are
	// case-insensitive. Read back by current_setting() / SHOW. nil until first set.
	settings map[string]string
}

// New constructs a session from a validated auth.Session and the PG startup
// parameters. The branch-targeting mechanism is the PG startup "branch"
// parameter (the integration review); the SDK/driver translates any ?branch= DSN form
// into this startup parameter.
func New(a auth.Session, connID uint64, params map[string]string) *Session {
	if params == nil {
		params = map[string]string{}
	}
	return &Session{Auth: a, connID: connID, params: params}
}

// Namespace returns the validated raw namespace identity for key encoding.
func (s *Session) Namespace() auth.NamespaceID { return s.Auth.Namespace }

// Branch returns the resolved branch ("main" if unset). The auth.Session.Branch
// claim takes precedence over the PG startup "branch" parameter; if neither is
// set, the connection operates on "main".
func (s *Session) Branch() string {
	if s.Auth.Branch != "" {
		return s.Auth.Branch
	}
	if s.overrideBranch != "" {
		return s.overrideBranch
	}
	if b, ok := s.params["branch"]; ok && b != "" {
		return b
	}
	return "main"
}

// SetBranch switches the connection's active branch (the `SET branch = '...'`
// statement). It is rejected when the session token pins a branch, so a scoped
// token cannot escape its branch.
func (s *Session) SetBranch(branch string) error {
	if s.Auth.Branch != "" {
		return fmt.Errorf("branch is pinned by the session token and cannot be changed")
	}
	s.overrideBranch = branch
	return nil
}

// ConnID returns the connection identifier.
func (s *Session) ConnID() uint64 { return s.connID }

// Param returns a PG startup parameter value and whether it was present.
func (s *Session) Param(key string) (string, bool) {
	v, ok := s.params[key]
	return v, ok
}

// SetSetting persists a run-time configuration parameter (a GUC) for this
// session. Used by SET and set_config(). GUC names are case-insensitive.
func (s *Session) SetSetting(key, value string) {
	if s.settings == nil {
		s.settings = map[string]string{}
	}
	s.settings[strings.ToLower(strings.TrimSpace(key))] = value
}

// GetSetting returns a session GUC value and whether it was set. Names are
// matched case-insensitively. Read by current_setting() and SHOW.
func (s *Session) GetSetting(key string) (string, bool) {
	if s.settings == nil {
		return "", false
	}
	v, ok := s.settings[strings.ToLower(strings.TrimSpace(key))]
	return v, ok
}

// ResetSetting clears a single session GUC (RESET name).
func (s *Session) ResetSetting(key string) {
	if s.settings == nil {
		return
	}
	delete(s.settings, strings.ToLower(strings.TrimSpace(key)))
}

// ResetAllSettings clears every session GUC (RESET ALL / DISCARD ALL).
func (s *Session) ResetAllSettings() { s.settings = nil }

// User returns the authenticated user identity for current_user / session_user.
// It prefers the PG startup "user" parameter (the role the client connected as,
// which is what RLS policies compare against), then the auth token subject, and
// falls back to "postgres" so RLS policies that compare against current_user
// have a stable value on an unauthenticated single-node session.
func (s *Session) User() string {
	if u, ok := s.params["user"]; ok && u != "" {
		return u
	}
	if s.Auth.Sub != "" {
		return s.Auth.Sub
	}
	return "postgres"
}

// IsBypassRLS reports whether this session bypasses Row-Level Security entirely
// (PostgreSQL: superusers and BYPASSRLS roles). The single-node engine treats an
// "admin" auth role as the bypass identity. When the role is not "admin", RLS is
// enforced — security is never silently skipped.
func (s *Session) IsBypassRLS() bool {
	return strings.EqualFold(s.Auth.Role, "admin")
}
