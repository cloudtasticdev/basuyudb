package auth

// DevSession builds a validated Session for local development / trust-auth
// connections (BASUYUDB_DEV_MODE=true only). It reuses SessionFromClaims so the
// namespace is validated through the same path as a real JWT-derived session —
// there is no second, weaker construction path for NamespaceID. The wire layer
// MUST gate any call to this behind BASUYUDB_DEV_MODE. (by design)
func DevSession(namespace, branch string) (Session, error) {
	s, err := SessionFromClaims(&SessionClaims{
		Sub: "dev",
		Jti: "dev",
		Role: "service",
		NamespaceAccess: []string{namespace},
		NamespaceID: namespace,
	})
	if err != nil {
		return Session{}, err
	}
	s.Branch = branch
	return s, nil
}
