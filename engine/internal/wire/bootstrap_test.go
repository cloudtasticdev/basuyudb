package wire

import (
	"path/filepath"
	"testing"
)

// TestBootstrapSeedsRole verifies that supplying BootstrapRole+BootstrapPassword
// seeds the role into the (persistent) store and that it survives a reload.
func TestBootstrapSeedsRole(t *testing.T) {
	rolesPath := filepath.Join(t.TempDir(), "roles.json")
	srv, err := NewServer(Config{
		Addr:              "127.0.0.1:0",
		RolesPath:         rolesPath,
		BootstrapRole:     "admin",
		BootstrapPassword: "s3cr3t",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !srv.roles.Has("admin") {
		t.Fatal("expected bootstrap role 'admin' to be seeded")
	}

	// Reload via a second server over the same persisted file: role persists,
	// and re-seeding the same role is a no-op (no error).
	srv2, err := NewServer(Config{
		Addr:              "127.0.0.1:0",
		RolesPath:         rolesPath,
		BootstrapRole:     "admin",
		BootstrapPassword: "different-now",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !srv2.roles.Has("admin") {
		t.Fatal("expected bootstrap role to persist across reload")
	}
}

// TestBootstrapRequiresBothCreds verifies seeding is skipped unless both the
// role and password are provided.
func TestBootstrapRequiresBothCreds(t *testing.T) {
	srv, err := NewServer(Config{
		Addr:          "127.0.0.1:0",
		RolesPath:     filepath.Join(t.TempDir(), "roles.json"),
		BootstrapRole: "admin", // password missing
	})
	if err != nil {
		t.Fatal(err)
	}
	if srv.roles.Has("admin") {
		t.Fatal("should not seed role without a password")
	}
}
