package session

import (
	"testing"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
)

func devSess(t *testing.T, branch string, params map[string]string) *Session {
	t.Helper()
	a, err := auth.DevSession("tenant-a", branch)
	if err != nil {
		t.Fatal(err)
	}
	return New(a, 1, params)
}

func TestSetBranchOverride(t *testing.T) {
	// Auth.Branch empty → default main, SET overrides.
	s := devSess(t, "", nil)
	if s.Branch() != "main" {
		t.Fatalf("default branch want main, got %q", s.Branch())
	}
	if err := s.SetBranch("feature-x"); err != nil {
		t.Fatalf("SetBranch: %v", err)
	}
	if s.Branch() != "feature-x" {
		t.Fatalf("after SET branch want feature-x, got %q", s.Branch())
	}
	// SET overrides the startup "branch" parameter.
	s2 := devSess(t, "", map[string]string{"branch": "param-branch"})
	_ = s2.SetBranch("set-branch")
	if s2.Branch() != "set-branch" {
		t.Fatalf("SET branch should override startup param, got %q", s2.Branch())
	}
}

func TestSetBranchRejectedWhenTokenPinned(t *testing.T) {
	// A token that pins a branch must not be overridable via SET (security).
	s := devSess(t, "pinned", nil)
	if err := s.SetBranch("other"); err == nil {
		t.Fatal("SET branch must be rejected when the token pins a branch")
	}
	if s.Branch() != "pinned" {
		t.Fatalf("branch must stay pinned, got %q", s.Branch())
	}
}
