// Package keymanager provides encryption-at-rest key management for BasuyuDB.
// In production it is backed by a GoHaSeMo Node (gohasemo.io/core SDK, MIT-licensed).
// Until the GoHaSeMo SDK is published the fallback implementation uses stdlib
// AES-256-GCM with per-namespace DEKs derived from a configured master key.
//
// GoHaSeMo ADR references:
// - ADR-001: pure Go, no CGO — both products share this constraint
// - ADR-005: envelope encryption with AAD-bound key material (mirrored here)
// - ADR-008: per-client KEK model (one namespace = one GoHaSeMo client registration)
// - ADR-013: embedded mode (GOHASEMO_URL unset → in-process Node)
package keymanager

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// BackupAAD is the Additional Authenticated Data bound to every backup ciphertext.
//
// Per GoHaSeMo ADR-005: the AAD is stored in plaintext alongside the ciphertext.
// Its integrity is enforced by the GCM authentication tag. Any modification to the
// stored AAD — including re-assigning a ciphertext from namespace A to namespace B —
// causes authentication failure at decrypt time. This is tamper-detection by design,
// not by policy.
//
// Field ordering is fixed and canonical. Adding new fields requires incrementing
// Version and adding a matching decode path in Serialize(). This mirrors the
// protobuf field-number discipline in GoHaSeMo ADR-005.
type BackupAAD struct {
	Version uint32 // AAD schema version; current: 1
	NamespaceID string // BasuyuDB namespace_id — cryptographic tenant isolation boundary
	BackupJobID string // unique per backup run — prevents AAD replay across jobs
	BackupType string // "full" | "incremental"
	CreatedAtNs int64 // unix nanoseconds — creation timestamp bound; cannot be retroactively altered
}

// Serialize produces a canonical, deterministic byte representation of the AAD.
//
// Encoding:
// - uint32 Version, big-endian
// - uint16-length-prefixed UTF-8 string fields in declared order
// - int64 CreatedAtNs, big-endian
//
// The choice of length-prefixed fields (not null-terminated, not JSON, not CBOR)
// mirrors GoHaSeMo ADR-005's canonical serialisation discipline: determinism is
// guaranteed by construction, not by parser behaviour.
//
// Any serialisation change — field added, field removed, type altered — must bump
// Version and add a corresponding decode path. Existing ciphertexts become unreadable
// if the AAD cannot be reproduced exactly; test with golden-byte fixtures in CI.
func (a BackupAAD) Serialize() ([]byte, error) {
	if a.Version == 0 {
		a.Version = 1
	}
	if a.NamespaceID == "" {
		return nil, fmt.Errorf("keymanager: BackupAAD.NamespaceID must not be empty")
	}
	if a.BackupJobID == "" {
		return nil, fmt.Errorf("keymanager: BackupAAD.BackupJobID must not be empty")
	}
	if a.BackupType != "full" && a.BackupType != "incremental" {
		return nil, fmt.Errorf("keymanager: BackupAAD.BackupType must be %q or %q, got %q",
			"full", "incremental", a.BackupType)
	}

	var buf bytes.Buffer

	// Field 1: Version (uint32, big-endian, 4 bytes)
	if err := binary.Write(&buf, binary.BigEndian, a.Version); err != nil {
		return nil, fmt.Errorf("keymanager: AAD serialise Version: %w", err)
	}
	// Field 2: NamespaceID (uint16 length prefix + UTF-8 bytes)
	if err := writeStringField(&buf, a.NamespaceID); err != nil {
		return nil, fmt.Errorf("keymanager: AAD serialise NamespaceID: %w", err)
	}
	// Field 3: BackupJobID
	if err := writeStringField(&buf, a.BackupJobID); err != nil {
		return nil, fmt.Errorf("keymanager: AAD serialise BackupJobID: %w", err)
	}
	// Field 4: BackupType
	if err := writeStringField(&buf, a.BackupType); err != nil {
		return nil, fmt.Errorf("keymanager: AAD serialise BackupType: %w", err)
	}
	// Field 5: CreatedAtNs (int64, big-endian, 8 bytes)
	if err := binary.Write(&buf, binary.BigEndian, a.CreatedAtNs); err != nil {
		return nil, fmt.Errorf("keymanager: AAD serialise CreatedAtNs: %w", err)
	}

	return buf.Bytes(), nil
}

// writeStringField writes a length-prefixed string to buf.
// Length is a uint16 (max 65535 bytes). Strings longer than 65535 bytes are rejected.
func writeStringField(buf *bytes.Buffer, s string) error {
	b := []byte(s)
	if len(b) > 65535 {
		return fmt.Errorf("string field exceeds maximum length of 65535 bytes (%d bytes)", len(b))
	}
	if err := binary.Write(buf, binary.BigEndian, uint16(len(b))); err != nil {
		return err
	}
	buf.Write(b)
	return nil
}
