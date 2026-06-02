package auth

import (
	"encoding/json"
	"fmt"
)

// SessionVarSetter is the interface that an executor must implement so that
// SetSessionClaims can inject JWT-derived session variables.
// The InMemoryExecutor in the wire package satisfies this interface.
type SessionVarSetter interface {
	SetSessionVar(key, value string)
}

// SetSessionClaims injects all 7 JWT-derived app.* session variables into the
// executor's session settings store. This follows the canonical contract defined
// in JWT_SESSION_VARIABLES.md.
//
// Variables injected:
// app.user_id ← jwt.sub
// app.role ← jwt.role
// app.namespace_access ← json-encoded jwt.namespace_access array
// app.namespace_id ← namespace_access[0] iff len==1, else ""
// app.jti ← jwt.jti
// app.email ← jwt.email (empty string if absent)
// app.azp ← jwt.azp (empty string if absent)
func SetSessionClaims(setter SessionVarSetter, claims *SessionClaims) error {
	if setter == nil {
		return fmt.Errorf("SetSessionClaims: setter is nil")
	}
	if claims == nil {
		return fmt.Errorf("SetSessionClaims: claims is nil")
	}

	nsAccessJSON, err := json.Marshal(claims.NamespaceAccess)
	if err != nil {
		return fmt.Errorf("marshal namespace_access: %w", err)
	}

	vars := map[string]string{
		"app.user_id": claims.Sub,
		"app.role": claims.Role,
		"app.namespace_access": string(nsAccessJSON),
		"app.namespace_id": claims.NamespaceID, // "" for multi-namespace tokens
		"app.jti": claims.Jti,
		"app.email": claims.Email, // "" if absent
		"app.azp": claims.Azp, // "" if absent
	}

	// Inject in deterministic order (important for test reproducibility).
	order := []string{
		"app.user_id",
		"app.role",
		"app.namespace_access",
		"app.namespace_id",
		"app.jti",
		"app.email",
		"app.azp",
	}
	for _, k := range order {
		setter.SetSessionVar(k, vars[k])
	}
	return nil
}

// InjectSessionVars is a convenience function that bridges the auth and wire
// packages: it calls SetSessionClaims on a SessionVarSetter.
func InjectSessionVars(setter SessionVarSetter, claims *SessionClaims) {
	// Errors from SetSessionClaims are non-fatal for connection setup — the
	// claims are already validated at this point. Log-only errors are handled
	// by the caller (handler.go).
	_ = SetSessionClaims(setter, claims)
}
