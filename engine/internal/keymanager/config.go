package keymanager

import "os"

// kmConfig holds all keymanager configuration parsed from environment variables.
type kmConfig struct {
	// GoHaSeMo Node address. If empty, the fallback manager is used.
	// When gohasemo.io/core SDK is published, a non-empty NodeURL selects the
	// GoHaSeMo-backed manager via the embedded or remote Node path.
	// See GoHaSeMo ADR-013 §3: embedded mode (NodeURL empty) vs remote mode.
	NodeURL string

	// SHA-256 fingerprint of the GoHaSeMo Node TLS certificate.
	// REQUIRED when NodeURL is set. GoHaSeMo ADR-008 Invariant 9:
	// TOFU is forbidden — the fingerprint must be pre-configured.
	NodeFingerprint string

	// Unique identifier for this BasuyuDB engine instance as a GoHaSeMo client.
	// Maps to one per-client KEK in the GoHaSeMo Node registry (ADR-008).
	// Default: "basuyudb-engine"
	ClientID string

	// Deployment environment. Set to "production" to enforce CapabilityBundle
	// requirement (GoHaSeMo ADR-013 §2: GOHASEMO_ENV=production forces bundle check).
	Env string

	// Master key for the stdlib fallback manager (hex-encoded 32 bytes = 64 chars).
	// Required for persistent ciphertexts across restarts in fallback mode.
	// If empty, an ephemeral key is generated (dev/CI acceptable; production not safe).
	MasterKeyHex string
}

func loadKMConfig() kmConfig {
	clientID := os.Getenv("GOHASEMO_CLIENT_ID")
	if clientID == "" {
		clientID = "basuyudb-engine"
	}
	return kmConfig{
		NodeURL: os.Getenv("GOHASEMO_URL"),
		NodeFingerprint: os.Getenv("GOHASEMO_NODE_FINGERPRINT"),
		ClientID: clientID,
		Env: os.Getenv("GOHASEMO_ENV"),
		MasterKeyHex: os.Getenv("BASUYUDB_KEYMANAGER_MASTER_KEY"),
	}
}
