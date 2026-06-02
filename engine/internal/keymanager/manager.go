package keymanager

import (
	"context"
	"log/slog"
)

// Manager is the key management interface for BasuyuDB encryption-at-rest operations.
//
// In production this is backed by a GoHaSeMo Node (gohasemo.io/core, MIT SDK).
// The GoHaSeMo-backed implementation provides:
// - Per-namespace KEK isolation via the GoHaSeMo per-client KEK registry (ADR-008)
// - VSM-protected root of trust — no passphrase-in-process (ADR-010)
// - Sentinel DR with configurable RPO ≤ 1 hour (ADR-011, satisfies PRD-010 RTO)
// - Post-quantum key transport via ML-KEM-768 (ADR-003)
// - Surgical revocation: RevokeNamespace destroys one KEK without affecting others
//
// Until gohasemo.io/core is published (GoHaSeMo Phase 1 → Development Starting),
// the fallback manager provides AES-256-GCM with per-namespace DEKs.
// The fallback satisfies PRD-010 "dedicated encryption key mandatory" at the
// implementation level; it does not satisfy GoHaSeMo-grade isolation guarantees.
type Manager interface {
	// EncryptBackup encrypts backup data for the given namespace.
	// aad.NamespaceID cryptographically binds the ciphertext to exactly one
	// namespace — decrypting under a different namespace's key fails authentication.
	// Returns nonce || GCM-ciphertext || GCM-tag (12 + n + 16 bytes).
	EncryptBackup(ctx context.Context, aad BackupAAD, plaintext []byte) ([]byte, error)

	// DecryptBackup decrypts backup data. The aad must reproduce the exact byte
	// representation used at encrypt time (same NamespaceID, BackupJobID, BackupType,
	// CreatedAtNs). Any mismatch causes authentication failure — this is ADR-005's
	// tamper-detection guarantee, not an error condition to be worked around.
	DecryptBackup(ctx context.Context, aad BackupAAD, ciphertext []byte) ([]byte, error)

	// ProvisionNamespace creates or retrieves the key material for a namespace.
	// Idempotent: safe to call multiple times for the same namespace.
	// In GoHaSeMo-backed mode: registers the namespace as a GoHaSeMo client,
	// establishing its independent KEK in the Node registry (ADR-008 §3).
	// In fallback mode: derives the namespace DEK from the master key (lazy, in-memory).
	ProvisionNamespace(ctx context.Context, namespaceID string) error

	// RevokeNamespace destroys all key material for the given namespace.
	// After this call, all ciphertexts encrypted under this namespace's key are
	// permanently unrecoverable — there is no undo.
	// In GoHaSeMo-backed mode: revokes the client KEK in the Node registry (surgical).
	// In fallback mode: removes the DEK from the in-process map (in-memory only).
	RevokeNamespace(ctx context.Context, namespaceID string) error

	// Close releases resources held by the manager (connections, goroutines, etc.).
	Close() error
}

// Mode identifies which key management backend is active.
type Mode string

const (
	// ModeGoHaSeMo indicates the GoHaSeMo Node SDK is active.
	// Provides VSM isolation, ML-KEM key transport, Sentinel DR, and surgical revocation.
	ModeGoHaSeMo Mode = "gohasemo"

	// ModeFallback indicates the stdlib AES-256-GCM fallback is active.
	// Adequate for dev/CI and interim production while GoHaSeMo SDK is not yet published.
	// Does NOT provide GoHaSeMo-grade key isolation.
	ModeFallback Mode = "fallback"
)

// New returns a Manager and the active Mode.
//
// Selection logic:
// 1. If GOHASEMO_URL is set AND gohasemo.io/core SDK is available → GoHaSeMo-backed.
// 2. Otherwise → stdlib fallback.
//
// Production gate: if GOHASEMO_ENV=production and the fallback is selected, a WARNING
// is logged but startup is NOT blocked. PRD-010 requires encryption-at-rest to be
// available; a blocking failure here would prevent recovery from a misconfiguration.
// Operators should monitor for the warning and remediate before claiming security-grade
// key isolation.
//
// TODO(gohasemo): When gohasemo.io/core is published (cloudtasticdev/gohasemo),
// add an import and the following selection here:
//
//	cfg := loadKMConfig()
//	if cfg.NodeURL != "" {
//	 if cfg.NodeFingerprint == "" {
//	 return nil, ModeGoHaSeMo, fmt.Errorf(
//	 "keymanager: GOHASEMO_NODE_FINGERPRINT is required when GOHASEMO_URL is set "+
//	 "(GoHaSeMo ADR-008 Invariant 9: TOFU is forbidden)")
//	 }
//	 m, err := newGoHaSeMoManager(cfg, logger)
//	 return m, ModeGoHaSeMo, err
//	}
func New(logger *slog.Logger) (Manager, Mode, error) {
	cfg := loadKMConfig()

	// GoHaSeMo-backed path: reserved for when SDK is published.
	// cfg.NodeURL check is here so the env var is parsed and logged even now.
	if cfg.NodeURL != "" {
		logger.Warn("keymanager: GOHASEMO_URL is set but gohasemo.io/core SDK is not yet published; "+
			"falling back to stdlib AES-256-GCM; "+
			"update to GoHaSeMo-backed mode when cloudtasticdev/gohasemo SDK is released",
			"gohasemo_url", cfg.NodeURL,
		)
	}

	if cfg.Env == "production" && cfg.NodeURL == "" {
		logger.Warn("keymanager: GOHASEMO_URL not set in production environment; "+
			"using local AES-256-GCM fallback; "+
			"this does not provide GoHaSeMo-grade key isolation (no VSM, no Sentinel DR, no surgical revocation); "+
			"set GOHASEMO_URL to a GoHaSeMo Node before claiming security-grade key management",
		)
	}

	m, err := newFallbackManager(cfg, logger)
	if err != nil {
		return nil, ModeFallback, err
	}
	return m, ModeFallback, nil
}
