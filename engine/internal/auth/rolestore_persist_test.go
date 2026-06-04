package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRoleStorePersistRoundTrip provisions a role, reloads the store from the
// same path/key, and verifies the verifier survives a full SCRAM exchange (and
// that a wrong password fails). Runs both unencrypted and encrypted.
func TestRoleStorePersistRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		encKey []byte
	}{
		{"plaintext", nil},
		{"encrypted", []byte("a-test-encryption-key-not-32-bytes")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "roles.json")

			rs1, err := NewPersistentRoleStore(path, tc.encKey)
			if err != nil {
				t.Fatal(err)
			}
			if err := rs1.UpsertPassword("dave", "hunter2"); err != nil {
				t.Fatal(err)
			}

			// Construct a brand-new store over the same path+key.
			rs2, err := NewPersistentRoleStore(path, tc.encKey)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			v, ok := rs2.Lookup("dave")
			if !ok {
				t.Fatal("expected dave to survive reload")
			}

			// Full SCRAM exchange against the reloaded verifier: right password OK.
			cl := &fakeClient{username: "dave", password: "hunter2", clientNonce: "nonceAAAA11112222"}
			if err := runExchange(t, v, cl); err != nil {
				t.Fatalf("SCRAM against reloaded verifier failed: %v", err)
			}
			// Wrong password must fail.
			bad := &fakeClient{username: "dave", password: "wrong", clientNonce: "nonceBBBB33334444"}
			if err := runExchange(t, v, bad); err == nil {
				t.Fatal("expected SCRAM failure for wrong password against reloaded verifier")
			}
		})
	}
}

// TestRoleStoreEncryptedAtRest verifies that with a key the on-disk bytes are
// NOT plaintext JSON (no username, not valid JSON), and without a key the file
// is plain JSON containing the username.
func TestRoleStoreEncryptedAtRest(t *testing.T) {
	t.Run("with-key", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "roles.json")
		rs, err := NewPersistentRoleStore(path, []byte("super-secret-key"))
		if err != nil {
			t.Fatal(err)
		}
		if err := rs.UpsertPassword("eve", "pw"); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "eve") {
			t.Fatal("encrypted file should not contain the username in plaintext")
		}
		var any map[string]interface{}
		if json.Unmarshal(raw, &any) == nil {
			t.Fatal("encrypted file should not be valid JSON")
		}
	})

	t.Run("without-key", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "roles.json")
		rs, err := NewPersistentRoleStore(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := rs.UpsertPassword("frank", "pw"); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc rolesDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("unencrypted file should be valid JSON: %v", err)
		}
		if _, ok := doc.Roles["frank"]; !ok {
			t.Fatal("expected role 'frank' in plaintext JSON")
		}
	})
}

// TestRoleStoreCorruptFile ensures a garbage encrypted file yields a load error
// rather than silently wiping credentials.
func TestRoleStoreCorruptFile(t *testing.T) {
	t.Run("undecryptable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "roles.json")
		if err := os.WriteFile(path, []byte("this-is-not-valid-ciphertext-at-all"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewPersistentRoleStore(path, []byte("a-key")); err == nil {
			t.Fatal("expected load error for undecryptable file, got nil (silent wipe)")
		}
	})

	t.Run("malformed-json", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "roles.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewPersistentRoleStore(path, nil); err == nil {
			t.Fatal("expected load error for malformed JSON, got nil")
		}
	})

	t.Run("missing-file-is-empty", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "does-not-exist.json")
		rs, err := NewPersistentRoleStore(path, nil)
		if err != nil {
			t.Fatalf("missing file should not error: %v", err)
		}
		if rs.Has("anyone") {
			t.Fatal("missing-file store should be empty")
		}
	})
}
