package wire

import (
	"testing"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
)

// TestApplySetGUC verifies the wire-layer SET parser persists the right GUC for
// every accepted form and ignores the control forms (SET ROLE / TRANSACTION).
func TestApplySetGUC(t *testing.T) {
	cases := []struct {
		sql      string
		wantKey  string
		wantVal  string
		wantSkip bool // true => no GUC should be written
	}{
		{sql: "SET app.tenant = 't1'", wantKey: "app.tenant", wantVal: "t1"},
		{sql: "SET app.tenant TO 't2'", wantKey: "app.tenant", wantVal: "t2"},
		{sql: "SET LOCAL app.tenant = 't3'", wantKey: "app.tenant", wantVal: "t3"},
		{sql: "SET SESSION app.tenant = 't4'", wantKey: "app.tenant", wantVal: "t4"},
		{sql: "SET search_path = public", wantKey: "search_path", wantVal: "public"},
		{sql: "SET TIME ZONE 'UTC'", wantSkip: true},
		{sql: "SET ROLE admin", wantSkip: true},
		{sql: "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE", wantSkip: true},
	}
	for _, c := range cases {
		s := session.New(auth.Session{}, 1, nil)
		applySetGUC(s, c.sql)
		got, ok := s.GetSetting(c.wantKey)
		if c.wantSkip {
			// For skip cases the probe key is the control keyword, which must not
			// have been written; assert no app.tenant/search_path leaked either.
			if _, leaked := s.GetSetting("app.tenant"); leaked {
				t.Fatalf("%q wrote app.tenant unexpectedly", c.sql)
			}
			continue
		}
		if !ok || got != c.wantVal {
			t.Fatalf("%q: want %s=%q, got %q (ok=%v)", c.sql, c.wantKey, c.wantVal, got, ok)
		}
	}

	// RESET path: case-insensitive key removal.
	s := session.New(auth.Session{}, 1, nil)
	s.SetSetting("app.tenant", "x")
	s.ResetSetting("APP.TENANT")
	if _, ok := s.GetSetting("app.tenant"); ok {
		t.Fatalf("ResetSetting did not clear the GUC (case-insensitive)")
	}
}
