// Package session is a NEUTRAL package that breaks the wire<->executor import
// cycle. It carries the validated auth.Session plus per-connection request
// state. executor imports session; executor does NOT import wire. wire builds a
// *session.Session and passes it to executor.Execute.
// (by design)
package session

import "github.com/cloudtasticdev/basuyudb/engine/internal/auth"

// Session is threaded wire -> executor -> storage. It derives its identity from
// auth.Session: it carries the validated RAW namespace string (NOT a hashed
// nsID). the design specs's old keyenc.SessionContext is DELETED and replaced by
// reads off this type. (by design)
type Session struct {
	Auth auth.Session // the validated capability token
	connID uint64
	params map[string]string // PG startup parameters (incl. "branch")
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
	if b, ok := s.params["branch"]; ok && b != "" {
		return b
	}
	return "main"
}

// ConnID returns the connection identifier.
func (s *Session) ConnID() uint64 { return s.connID }

// Param returns a PG startup parameter value and whether it was present.
func (s *Session) Param(key string) (string, bool) {
	v, ok := s.params[key]
	return v, ok
}
