package executor

import (
	"strings"
	"testing"
)

// collectFirstCol returns the first-column text of every result row.
func collectFirstCol(res *Result) []string {
	out := make([]string, 0, len(res.Rows))
	for _, r := range res.Rows {
		out = append(out, r[0].Text)
	}
	return out
}

func sameSet(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		seen[w]--
	}
	for _, v := range seen {
		if v != 0 {
			return false
		}
	}
	return true
}

// TestRLSTenantIsolation proves enforcement (a): a USING policy keyed on a
// session GUC filters SELECT to the current tenant, and changing the GUC changes
// the visible rows. This is the core multi-tenant SaaS guarantee.
func TestRLSTenantIsolation(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE docs (id INT PRIMARY KEY, tenant TEXT, body TEXT)")
	run(t, ex, sess, "INSERT INTO docs (id, tenant, body) VALUES (1, 't1', 'a')")
	run(t, ex, sess, "INSERT INTO docs (id, tenant, body) VALUES (2, 't2', 'b')")
	run(t, ex, sess, "INSERT INTO docs (id, tenant, body) VALUES (3, 't1', 'c')")

	run(t, ex, sess, "CREATE POLICY tenant_iso ON docs USING (tenant = current_setting('app.tenant'))")
	run(t, ex, sess, "ALTER TABLE docs ENABLE ROW LEVEL SECURITY")

	sess.SetSetting("app.tenant", "t1")
	res := run(t, ex, sess, "SELECT id FROM docs")
	if got := collectFirstCol(res); !sameSet(got, "1", "3") {
		t.Fatalf("tenant=t1 want rows {1,3}, got %v", got)
	}

	sess.SetSetting("app.tenant", "t2")
	res = run(t, ex, sess, "SELECT id FROM docs")
	if got := collectFirstCol(res); !sameSet(got, "2") {
		t.Fatalf("tenant=t2 want rows {2}, got %v", got)
	}
}

// TestRLSDefaultDeny proves enforcement (b): RLS enabled with no applicable
// policy yields zero rows (PostgreSQL default-deny), not all rows.
func TestRLSDefaultDeny(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE secret (id INT PRIMARY KEY, v TEXT)")
	run(t, ex, sess, "INSERT INTO secret (id, v) VALUES (1, 'x')")
	run(t, ex, sess, "INSERT INTO secret (id, v) VALUES (2, 'y')")

	// Visible before enabling RLS.
	if res := run(t, ex, sess, "SELECT id FROM secret"); len(res.Rows) != 2 {
		t.Fatalf("pre-RLS want 2 rows, got %d", len(res.Rows))
	}

	run(t, ex, sess, "ALTER TABLE secret ENABLE ROW LEVEL SECURITY")

	if res := run(t, ex, sess, "SELECT id FROM secret"); len(res.Rows) != 0 {
		t.Fatalf("default-deny want 0 rows, got %d (%v)", len(res.Rows), collectFirstCol(res))
	}
}

// TestRLSInsertWithCheck proves enforcement (c): an INSERT violating WITH CHECK
// is rejected with SQLSTATE 42501, while a conforming INSERT succeeds.
func TestRLSInsertWithCheck(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE rows1 (id INT PRIMARY KEY, tenant TEXT)")
	run(t, ex, sess, "CREATE POLICY p ON rows1 USING (tenant = current_setting('app.tenant')) WITH CHECK (tenant = current_setting('app.tenant'))")
	run(t, ex, sess, "ALTER TABLE rows1 ENABLE ROW LEVEL SECURITY")
	sess.SetSetting("app.tenant", "t1")

	// Conforming insert allowed.
	run(t, ex, sess, "INSERT INTO rows1 (id, tenant) VALUES (1, 't1')")

	// Wrong-tenant insert violates WITH CHECK → 42501.
	err := execErr(ex, sess, "INSERT INTO rows1 (id, tenant) VALUES (2, 't2')")
	if err == nil {
		t.Fatalf("expected WITH CHECK violation")
	}
	if !strings.Contains(err.Error(), "42501") || !strings.Contains(err.Error(), "row-level security") {
		t.Fatalf("expected 42501 row-level security error, got %v", err)
	}
}

// TestRLSInsertWithCheckFallsBackToUsing proves the PostgreSQL rule that, for
// INSERT, an omitted WITH CHECK falls back to the policy's USING predicate.
func TestRLSInsertWithCheckFallsBackToUsing(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE rows2 (id INT PRIMARY KEY, tenant TEXT)")
	run(t, ex, sess, "CREATE POLICY p ON rows2 USING (tenant = current_setting('app.tenant'))")
	run(t, ex, sess, "ALTER TABLE rows2 ENABLE ROW LEVEL SECURITY")
	sess.SetSetting("app.tenant", "t1")

	run(t, ex, sess, "INSERT INTO rows2 (id, tenant) VALUES (1, 't1')")
	if err := execErr(ex, sess, "INSERT INTO rows2 (id, tenant) VALUES (2, 't2')"); err == nil {
		t.Fatalf("expected USING to serve as WITH CHECK for INSERT")
	} else if !strings.Contains(err.Error(), "42501") {
		t.Fatalf("expected 42501, got %v", err)
	}
}

// TestRLSPermissiveOr proves enforcement (d, permissive): multiple PERMISSIVE
// policies are OR-combined — a row visible to either policy is returned.
func TestRLSPermissiveOr(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE p_or (id INT PRIMARY KEY, tag TEXT)")
	run(t, ex, sess, "INSERT INTO p_or (id, tag) VALUES (1, 'red')")
	run(t, ex, sess, "INSERT INTO p_or (id, tag) VALUES (2, 'blue')")
	run(t, ex, sess, "INSERT INTO p_or (id, tag) VALUES (3, 'green')")

	run(t, ex, sess, "CREATE POLICY pr ON p_or USING (tag = 'red')")
	run(t, ex, sess, "CREATE POLICY pb ON p_or USING (tag = 'blue')")
	run(t, ex, sess, "ALTER TABLE p_or ENABLE ROW LEVEL SECURITY")

	res := run(t, ex, sess, "SELECT id FROM p_or")
	if got := collectFirstCol(res); !sameSet(got, "1", "2") {
		t.Fatalf("permissive OR want {1,2}, got %v", got)
	}
}

// TestRLSRestrictiveAnd proves enforcement (d, restrictive): a RESTRICTIVE
// policy is AND-combined with the permissive set, further narrowing the result.
func TestRLSRestrictiveAnd(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE p_and (id INT PRIMARY KEY, tenant TEXT, archived TEXT)")
	run(t, ex, sess, "INSERT INTO p_and (id, tenant, archived) VALUES (1, 't1', 'no')")
	run(t, ex, sess, "INSERT INTO p_and (id, tenant, archived) VALUES (2, 't1', 'yes')")
	run(t, ex, sess, "INSERT INTO p_and (id, tenant, archived) VALUES (3, 't2', 'no')")

	// Permissive: same tenant. Restrictive: not archived.
	run(t, ex, sess, "CREATE POLICY perm ON p_and USING (tenant = current_setting('app.tenant'))")
	run(t, ex, sess, "CREATE POLICY restr ON p_and AS RESTRICTIVE USING (archived = 'no')")
	run(t, ex, sess, "ALTER TABLE p_and ENABLE ROW LEVEL SECURITY")
	sess.SetSetting("app.tenant", "t1")

	res := run(t, ex, sess, "SELECT id FROM p_and")
	// tenant=t1 → {1,2}; AND not archived → {1}.
	if got := collectFirstCol(res); !sameSet(got, "1") {
		t.Fatalf("restrictive AND want {1}, got %v", got)
	}
}

// TestCurrentSettingRoundTrip proves enforcement (e): current_setting reads what
// SET (via SetSetting) and set_config wrote, including custom namespaced GUCs.
func TestCurrentSettingRoundTrip(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	// set_config writes to the session; current_setting reads it back.
	res := run(t, ex, sess, "SELECT set_config('app.tenant', 'acme', false)")
	if res.Rows[0][0].Text != "acme" {
		t.Fatalf("set_config returns value, got %q", res.Rows[0][0].Text)
	}
	res = run(t, ex, sess, "SELECT current_setting('app.tenant')")
	if res.Rows[0][0].Text != "acme" {
		t.Fatalf("current_setting after set_config want acme, got %q", res.Rows[0][0].Text)
	}

	// A value set directly on the session (the SET path) is also readable.
	sess.SetSetting("app.region", "eu")
	res = run(t, ex, sess, "SELECT current_setting('app.region')")
	if res.Rows[0][0].Text != "eu" {
		t.Fatalf("current_setting want eu, got %q", res.Rows[0][0].Text)
	}

	// Unknown GUC without missing_ok errors (42704); with missing_ok → NULL.
	if err := execErr(ex, sess, "SELECT current_setting('app.nope')"); err == nil {
		t.Fatalf("expected error for unknown GUC")
	} else if !strings.Contains(err.Error(), "42704") {
		t.Fatalf("expected 42704, got %v", err)
	}
	res = run(t, ex, sess, "SELECT current_setting('app.nope', true)")
	if !res.Rows[0][0].Null {
		t.Fatalf("current_setting missing_ok want NULL, got %q", res.Rows[0][0].Text)
	}
}

// TestRLSUpdateDeleteVisibility proves USING governs which rows UPDATE and DELETE
// may touch: a row hidden by policy is silently skipped, not modified.
func TestRLSUpdateDeleteVisibility(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE ud (id INT PRIMARY KEY, tenant TEXT, v TEXT)")
	run(t, ex, sess, "INSERT INTO ud (id, tenant, v) VALUES (1, 't1', 'a')")
	run(t, ex, sess, "INSERT INTO ud (id, tenant, v) VALUES (2, 't2', 'b')")
	run(t, ex, sess, "CREATE POLICY p ON ud USING (tenant = current_setting('app.tenant'))")
	run(t, ex, sess, "ALTER TABLE ud ENABLE ROW LEVEL SECURITY")
	sess.SetSetting("app.tenant", "t1")

	// UPDATE only affects the visible (t1) row.
	res := run(t, ex, sess, "UPDATE ud SET v = 'z' WHERE id IN (1, 2)")
	if res.RowsAffected != 1 {
		t.Fatalf("UPDATE under RLS want 1 row affected, got %d", res.RowsAffected)
	}
	// DELETE likewise cannot remove the hidden t2 row.
	res = run(t, ex, sess, "DELETE FROM ud WHERE id IN (1, 2)")
	if res.RowsAffected != 1 {
		t.Fatalf("DELETE under RLS want 1 row affected, got %d", res.RowsAffected)
	}
}

// TestRLSPgPoliciesView proves the pg_policies introspection view exposes the
// stored policies for tooling and tests.
func TestRLSPgPoliciesView(t *testing.T) {
	ex, done := newIdxExec(t)
	defer done()
	sess := testSession(t)

	run(t, ex, sess, "CREATE TABLE v (id INT PRIMARY KEY, tenant TEXT)")
	run(t, ex, sess, "CREATE POLICY vp ON v USING (tenant = current_setting('app.tenant'))")
	run(t, ex, sess, "ALTER TABLE v ENABLE ROW LEVEL SECURITY")

	res := run(t, ex, sess, `SELECT policyname, "permissive", cmd FROM pg_catalog.pg_policies WHERE tablename = 'v'`)
	if len(res.Rows) != 1 {
		t.Fatalf("pg_policies want 1 row, got %d", len(res.Rows))
	}
	if res.Rows[0][0].Text != "vp" || res.Rows[0][1].Text != "PERMISSIVE" || res.Rows[0][2].Text != "ALL" {
		t.Fatalf("pg_policies row unexpected: %v", res.Rows[0])
	}
}
