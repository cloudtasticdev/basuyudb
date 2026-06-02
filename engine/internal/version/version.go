// Package version is the single source of truth for the BasuyuDB engine
// version. Update Number here on each release; the release tag (vX.Y.Z) must
// match Number.
package version

// Number is the semantic version of the engine (no leading "v").
const Number = "0.4.0"

// PGWireServerVersion is the value reported for the PostgreSQL `server_version`
// parameter and version() — it advertises PG-15 wire compatibility plus the
// BasuyuDB engine version.
const PGWireServerVersion = "15.0 (BasuyuDB " + Number + ")"
