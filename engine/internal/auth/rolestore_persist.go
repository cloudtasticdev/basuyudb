package auth

// Durable, optionally-encrypted persistence backend for the RoleStore.
//
// The on-disk format is JSON: a map of username -> base64-encoded verifier
// fields (salt/iterations/StoredKey/ServerKey). We persist ONLY the SCRAM
// verifier — never the plaintext password — exactly as the in-memory store
// keeps it. This mirrors PostgreSQL's pg_authid, which lives inside the data
// directory and holds password hashes (SCRAM verifiers), not plaintext.
//
// Encryption at rest: when an encryption key is supplied (the same
// BASUYUDB_ENCRYPTION_KEY used for BadgerDB), the JSON document is encrypted
// with AES-256-GCM. The 12-byte random nonce is prepended to the ciphertext.
// The key is derived to exactly 32 bytes via SHA-256 of the provided key when
// it is not already 32 bytes long. Without a key, the file is written as plain
// JSON.
//
// Writes are atomic: we write to a sibling temp file and rename it over the
// target, so a crash mid-write cannot corrupt the live store. A missing file
// loads as an empty store; a present-but-undecryptable/corrupt file is a hard
// error at load time (we never silently wipe credentials).

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// persistedVerifier is the JSON shape of a single role's stored verifier. All
// binary fields are base64 (std encoding) so the document is plain text.
type persistedVerifier struct {
	Salt       string `json:"salt"`
	Iterations int    `json:"iterations"`
	StoredKey  string `json:"stored_key"`
	ServerKey  string `json:"server_key"`
}

// rolesDoc is the top-level on-disk document: username -> verifier.
type rolesDoc struct {
	Roles map[string]persistedVerifier `json:"roles"`
}

// persistence holds the configuration for a durable RoleStore backend.
type persistence struct {
	path   string
	encKey []byte // nil/empty => plaintext JSON; else AES-256-GCM
}

// deriveAESKey returns a 32-byte AES-256 key from the provided key material. If
// the material is already exactly 32 bytes it is used as-is; otherwise it is
// hashed with SHA-256 (which yields 32 bytes).
func deriveAESKey(key []byte) []byte {
	if len(key) == 32 {
		out := make([]byte, 32)
		copy(out, key)
		return out
	}
	sum := sha256.Sum256(key)
	return sum[:]
}

// encrypt seals plaintext with AES-256-GCM, prepending the random nonce.
func (p *persistence) encrypt(plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(deriveAESKey(p.encKey))
	if err != nil {
		return nil, fmt.Errorf("auth: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("auth: gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("auth: nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// decrypt opens an AES-256-GCM blob produced by encrypt.
func (p *persistence) decrypt(blob []byte) ([]byte, error) {
	block, err := aes.NewCipher(deriveAESKey(p.encKey))
	if err != nil {
		return nil, fmt.Errorf("auth: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("auth: gcm: %w", err)
	}
	ns := gcm.NonceSize()
	if len(blob) < ns {
		return nil, errors.New("auth: roles file truncated (shorter than nonce)")
	}
	nonce, ct := blob[:ns], blob[ns:]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("auth: roles file decryption failed (wrong key or corrupt): %w", err)
	}
	return pt, nil
}

// encrypted reports whether this backend encrypts at rest.
func (p *persistence) encrypted() bool {
	return len(p.encKey) > 0
}

// load reads and decodes the roles map from disk. A missing file yields an
// empty map and no error. Any other failure (unreadable, undecryptable, or
// malformed) is returned as an error so the caller can fail fast rather than
// silently start with no credentials.
func (p *persistence) load() (map[string]SCRAMVerifier, error) {
	raw, err := os.ReadFile(p.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return make(map[string]SCRAMVerifier), nil
		}
		return nil, fmt.Errorf("auth: read roles file %q: %w", p.path, err)
	}
	if len(raw) == 0 {
		return make(map[string]SCRAMVerifier), nil
	}

	data := raw
	if p.encrypted() {
		data, err = p.decrypt(raw)
		if err != nil {
			return nil, err
		}
	}

	var doc rolesDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("auth: parse roles file %q: %w", p.path, err)
	}
	out := make(map[string]SCRAMVerifier, len(doc.Roles))
	for name, pv := range doc.Roles {
		v, err := pv.toVerifier()
		if err != nil {
			return nil, fmt.Errorf("auth: decode verifier for role %q: %w", name, err)
		}
		out[name] = v
	}
	return out, nil
}

// save serialises the roles map and writes it atomically (temp file + rename).
// Callers MUST hold the RoleStore lock so the snapshot is consistent.
func (p *persistence) save(roles map[string]SCRAMVerifier) error {
	doc := rolesDoc{Roles: make(map[string]persistedVerifier, len(roles))}
	for name, v := range roles {
		doc.Roles[name] = fromVerifier(v)
	}
	plain, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("auth: marshal roles: %w", err)
	}

	out := plain
	if p.encrypted() {
		out, err = p.encrypt(plain)
		if err != nil {
			return err
		}
	}

	dir := filepath.Dir(p.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("auth: create roles dir %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".roles-*.tmp")
	if err != nil {
		return fmt.Errorf("auth: create temp roles file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we don't make it to a successful rename.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("auth: write temp roles file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("auth: sync temp roles file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("auth: close temp roles file: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("auth: chmod temp roles file: %w", err)
	}
	if err := os.Rename(tmpName, p.path); err != nil {
		return fmt.Errorf("auth: rename roles file into place: %w", err)
	}
	return nil
}

func fromVerifier(v SCRAMVerifier) persistedVerifier {
	enc := base64.StdEncoding.EncodeToString
	return persistedVerifier{
		Salt:       enc(v.Salt),
		Iterations: v.Iterations,
		StoredKey:  enc(v.StoredKey),
		ServerKey:  enc(v.ServerKey),
	}
}

func (pv persistedVerifier) toVerifier() (SCRAMVerifier, error) {
	dec := base64.StdEncoding.DecodeString
	salt, err := dec(pv.Salt)
	if err != nil {
		return SCRAMVerifier{}, fmt.Errorf("salt: %w", err)
	}
	storedKey, err := dec(pv.StoredKey)
	if err != nil {
		return SCRAMVerifier{}, fmt.Errorf("stored_key: %w", err)
	}
	serverKey, err := dec(pv.ServerKey)
	if err != nil {
		return SCRAMVerifier{}, fmt.Errorf("server_key: %w", err)
	}
	return SCRAMVerifier{
		Salt:       salt,
		Iterations: pv.Iterations,
		StoredKey:  storedKey,
		ServerKey:  serverKey,
	}, nil
}
