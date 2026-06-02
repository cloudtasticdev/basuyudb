package auth

import (
	"fmt"
	"regexp"
)

// This file defines the canonical capability token (Session) and namespace
// identity (NamespaceID) per the design specs §5 (by design).
//
// Conformed to the design specs (the reconciliation pass). Resolves the architecture review (one session
// token type) and the architecture review (raw validated namespace identity, never hashed).
//
// The pre-existing SessionClaims/JWKS machinery (jwks.go) performs the JWT
// signature + algorithm + temporal validation. This file layers the typed
// identity contract on top of it: a NamespaceID can be constructed ONLY by
// validating a namespace string, and a Session is derived ONLY from already
// JWKS-verified SessionClaims. This enforces the auth-before-branch invariant.

// namespaceRe is the canonical namespace identifier whitelist (a design decision,
// the architecture review aligned to the broader 128-char key-encoding bound).
var namespaceRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// NamespaceID is the validated raw namespace string (the namespace_access UUID).
// It is NOT a hash. Its field is unexported so it cannot be forged: the only
// constructor is newNamespaceID, reached via SessionFromClaims /
// (*JWKSCache).ValidateNamespace. (by design)
type NamespaceID struct {
	raw string // validated against [a-zA-Z0-9_-]{1,128}
}

// String returns the raw validated namespace string for key encoding.
func (n NamespaceID) String() string { return n.raw }

// IsZero reports whether the NamespaceID is the zero value (unvalidated).
func (n NamespaceID) IsZero() bool { return n.raw == "" }

// newNamespaceID is the sole NamespaceID constructor. It validates raw against
// the canonical whitelist. Unexported by design.
func newNamespaceID(raw string) (NamespaceID, error) {
	if !namespaceRe.MatchString(raw) {
		return NamespaceID{}, fmt.Errorf("invalid namespace identifier %q: must match [a-zA-Z0-9_-]{1,128}", raw)
	}
	return NamespaceID{raw: raw}, nil
}

// Session is THE capability token, produced from a verified PassportAuth JWT.
// It is the single session type threaded through wire -> executor -> storage.
// (by design)
type Session struct {
	Namespace NamespaceID // validated raw namespace (from namespace_access)
	Branch string // requested branch; "main" if unset
	Sub string // JWT subject
	JTI string // JWT ID
	AZP string // authorized party
	Role string // "user" | "admin" | "service"
}

// SessionFromClaims constructs a Session from already JWKS-verified
// SessionClaims. The namespace source is the single-entry namespace_access
// claim (the architecture review): SessionClaims.NamespaceID is populated only when the token
// grants exactly one namespace. A multi-namespace or wildcard ("*") token has
// no single operating namespace and is rejected here — the engine serves one
// namespace per connection. (by design)
func SessionFromClaims(claims *SessionClaims) (Session, error) {
	if claims == nil {
		return Session{}, fmt.Errorf("SessionFromClaims: claims is nil")
	}
	if claims.NamespaceID == "" {
		return Session{}, fmt.Errorf("SessionFromClaims: token does not grant exactly one namespace (namespace_access=%v); a single operating namespace is required", claims.NamespaceAccess)
	}
	nsID, err := newNamespaceID(claims.NamespaceID)
	if err != nil {
		return Session{}, err
	}
	return Session{
		Namespace: nsID,
		Sub: claims.Sub,
		JTI: claims.Jti,
		AZP: claims.Azp,
		Role: claims.Role,
	}, nil
}

// ValidateNamespace verifies the JWT against the JWKS cache and constructs the
// Session (and its NamespaceID) from the single-entry namespace_access claim.
// It is the ONLY way to obtain a NamespaceID from a raw token, enforcing
// auth-before-branch. Returns an error if the JWT is invalid or namespace_access
// is missing / multi-valued / malformed. (by design)
func (c *JWKSCache) ValidateNamespace(tokenStr string) (Session, error) {
	claims, err := c.ValidateToken(tokenStr)
	if err != nil {
		return Session{}, fmt.Errorf("jwt validation failed: %w", err)
	}
	return SessionFromClaims(claims)
}
