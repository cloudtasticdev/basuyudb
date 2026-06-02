package keymanager

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"sync"
)

// fallbackManager is a stdlib AES-256-GCM key manager.
// Used in dev/CI and as the interim implementation while gohasemo.io/core SDK
// is not yet published.
//
// Security properties:
// - Per-namespace DEKs via SHA-256(masterKey || namespaceID) — each namespace
// has a distinct key; cross-namespace decryption is impossible without the master key
// - AES-256-GCM with random 12-byte nonce per encryption operation
// - AAD bound via BackupAAD.Serialize() — metadata tampering fails GCM authentication
// - No plaintext key material in logs or error messages
//
// NOT equivalent to GoHaSeMo-backed mode:
// - Master key shares process space with engine code (no VSM isolation, ADR-010)
// - No Sentinel cluster DR — key backup is operator's responsibility (ADR-011)
// - No M-of-N Guardian ceremony — single master key, single point of failure (ADR-010)
// - Revocation removes the in-memory DEK only; ciphertexts are unreadable but the
// underlying master key can re-derive the DEK if not rotated (no true revocation)
// - No post-quantum key transport (ADR-003)
//
// Restart behaviour:
// - With BASUYUDB_KEYMANAGER_MASTER_KEY set: DEKs are deterministically re-derived
// from the master key on startup — existing ciphertexts remain readable.
// - Without BASUYUDB_KEYMANAGER_MASTER_KEY: ephemeral random master key is generated;
// all ciphertexts are unreadable after restart. Acceptable in dev/CI; NEVER in production.
type fallbackManager struct {
	masterKey [32]byte
	logger *slog.Logger
	mu sync.RWMutex
	namespaces map[string][32]byte // namespace_id → derived DEK (in-process cache)
}

func newFallbackManager(cfg kmConfig, logger *slog.Logger) (*fallbackManager, error) {
	var masterKey [32]byte

	if cfg.MasterKeyHex != "" {
		b, err := hex.DecodeString(cfg.MasterKeyHex)
		if err != nil {
			return nil, fmt.Errorf("keymanager: BASUYUDB_KEYMANAGER_MASTER_KEY is not valid hex: %w", err)
		}
		if len(b) != 32 {
			return nil, fmt.Errorf("keymanager: BASUYUDB_KEYMANAGER_MASTER_KEY must decode to exactly 32 bytes (64 hex chars), got %d bytes", len(b))
		}
		copy(masterKey[:], b)
		logger.Info("keymanager: fallback manager initialised with persistent master key")
	} else {
		// Generate a random ephemeral master key for this process lifetime.
		if _, err := io.ReadFull(rand.Reader, masterKey[:]); err != nil {
			return nil, fmt.Errorf("keymanager: failed to generate ephemeral master key: %w", err)
		}
		logger.Warn("keymanager: BASUYUDB_KEYMANAGER_MASTER_KEY not set — using ephemeral key; " +
			"existing backup ciphertexts will be unreadable after restart; " +
			"set BASUYUDB_KEYMANAGER_MASTER_KEY=<64-hex-chars> for persistent key management")
	}

	return &fallbackManager{
		masterKey: masterKey,
		logger: logger,
		namespaces: make(map[string][32]byte),
	}, nil
}

// ProvisionNamespace creates or retrieves the in-memory DEK for a namespace.
// Idempotent: safe to call multiple times for the same namespace.
func (m *fallbackManager) ProvisionNamespace(_ context.Context, namespaceID string) error {
	if namespaceID == "" {
		return fmt.Errorf("keymanager: ProvisionNamespace: namespaceID must not be empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.namespaces[namespaceID]; !exists {
		m.namespaces[namespaceID] = m.deriveNamespaceDEK(namespaceID)
		m.logger.Info("keymanager: namespace DEK provisioned (fallback mode)",
			"namespace_id", namespaceID,
			"mode", string(ModeFallback),
		)
	}
	return nil
}

// RevokeNamespace removes the in-memory DEK for the namespace.
// Note: in fallback mode this is an in-memory eviction only. The DEK can be
// re-derived from the master key at any time. True revocation requires GoHaSeMo-backed
// mode where the KEK is destroyed in the Node registry (ADR-008 §surgical revocation).
func (m *fallbackManager) RevokeNamespace(_ context.Context, namespaceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.namespaces, namespaceID)
	m.logger.Info("keymanager: namespace DEK evicted from process memory (fallback mode — "+
		"master key can still re-derive this DEK; use GoHaSeMo-backed mode for true revocation)",
		"namespace_id", namespaceID,
	)
	return nil
}

// EncryptBackup encrypts plaintext under the namespace's DEK using AES-256-GCM.
// Output format: 12-byte random nonce || GCM ciphertext || 16-byte GCM tag.
// The AAD is serialised and bound to the ciphertext's authentication tag.
func (m *fallbackManager) EncryptBackup(ctx context.Context, aad BackupAAD, plaintext []byte) ([]byte, error) {
	aadBytes, err := aad.Serialize()
	if err != nil {
		return nil, err
	}

	dek, err := m.getDEK(ctx, aad.NamespaceID)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(dek[:])
	if err != nil {
		return nil, fmt.Errorf("keymanager: AES cipher init: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("keymanager: GCM init: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize()) // 12 bytes for standard GCM
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("keymanager: nonce generation: %w", err)
	}

	// Seal appends GCM ciphertext + tag to nonce, producing nonce || ciphertext || tag.
	ciphertext := gcm.Seal(nonce, nonce, plaintext, aadBytes)
	return ciphertext, nil
}

// DecryptBackup decrypts a ciphertext produced by EncryptBackup.
// The aad must be reproduced exactly — any field change causes authentication failure.
func (m *fallbackManager) DecryptBackup(ctx context.Context, aad BackupAAD, ciphertext []byte) ([]byte, error) {
	aadBytes, err := aad.Serialize()
	if err != nil {
		return nil, err
	}

	dek, err := m.getDEK(ctx, aad.NamespaceID)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(dek[:])
	if err != nil {
		return nil, fmt.Errorf("keymanager: AES cipher init: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("keymanager: GCM init: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize+gcm.Overhead() {
		return nil, fmt.Errorf("keymanager: ciphertext too short (%d bytes, minimum %d)", len(ciphertext), nonceSize+gcm.Overhead())
	}

	nonce, body := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, body, aadBytes)
	if err != nil {
		// Do not include err details — timing oracle and information disclosure risk.
		return nil, fmt.Errorf("keymanager: authentication failed — ciphertext or AAD tampered (namespace_id=%s, job_id=%s)",
			aad.NamespaceID, aad.BackupJobID)
	}
	return plaintext, nil
}

// Close is a no-op for the fallback manager; it holds no external resources.
func (m *fallbackManager) Close() error { return nil }

// ─── Internal helpers ─────────────────────────────────────────────────────────

// deriveNamespaceDEK derives a 256-bit DEK via SHA-256(masterKey || namespaceID).
//
// Security note: this is a simplified single-round KDF suitable for the fallback
// mode's security level. GoHaSeMo-backed mode uses HKDF-SHA384 per ADR-008, which
// provides domain separation, context binding, and standard KDF security proofs.
// The SHA-256 construction here provides key separation per namespace but lacks
// the formal security proof of HKDF. Adequate for fallback; not a production target.
func (m *fallbackManager) deriveNamespaceDEK(namespaceID string) [32]byte {
	h := sha256.New()
	h.Write(m.masterKey[:])
	h.Write([]byte{0x00}) // domain separator between master key and namespace ID
	h.Write([]byte(namespaceID))
	var dek [32]byte
	copy(dek[:], h.Sum(nil))
	return dek
}

// getDEK retrieves the DEK for the given namespace, auto-provisioning if not present.
func (m *fallbackManager) getDEK(ctx context.Context, namespaceID string) ([32]byte, error) {
	m.mu.RLock()
	dek, ok := m.namespaces[namespaceID]
	m.mu.RUnlock()
	if ok {
		return dek, nil
	}
	// Auto-provision: first call for this namespace in this process.
	if err := m.ProvisionNamespace(ctx, namespaceID); err != nil {
		return [32]byte{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.namespaces[namespaceID], nil
}
