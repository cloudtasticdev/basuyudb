package executor

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// Row-Level Security (RLS) execution.
//
// Catalog: RLS state (rls_enabled, rls_forced) and the table's policy list are
// persisted inline in tableSchema (see catalog.go), so they travel with the
// schema and need no separate keyspace. Policy predicates are stored as deparsed
// SQL text and re-parsed at enforcement time, exactly like CHECK constraints.
//
// Enforcement model (PostgreSQL semantics, verified against the official docs):
//   - A row is visible/targetable iff
//       (OR of applicable PERMISSIVE USING) AND (AND of applicable RESTRICTIVE USING).
//   - If RLS is enabled but NO policy applies to the command/role, the result is
//     default-DENY (zero rows; INSERT rejected).
//   - INSERT/UPDATE validate the NEW row against WITH CHECK; when a policy omits
//     WITH CHECK, its USING predicate serves as the check.
//   - The table owner is exempt unless FORCE ROW LEVEL SECURITY is set; a
//     BYPASSRLS/admin session bypasses entirely. This single-node engine has no
//     per-table owner, so only the admin (BYPASSRLS) session is exempt; FORCE is
//     honored for completeness. When in doubt we ENFORCE — RLS is never silently
//     skipped while enabled.

// persistSchema writes a (possibly RLS-modified) schema back to the catalog.
func (e *execImpl) persistSchema(txn *transactions.Txn, sess *session.Session, sch *tableSchema) error {
	raw, err := json.Marshal(sch)
	if err != nil {
		return newExecError("XX000", "encode schema: %v", err)
	}
	e.txn.Buffer(txn, transactions.Mutation{Key: e.store.Encoder().SchemaKey(sess.Namespace(), sch.Name), Value: raw})
	return nil
}

// execCreatePolicy adds a policy to a table's policy list (CREATE POLICY).
func (e *execImpl) execCreatePolicy(ctx context.Context, s *ast.CreatePolicyStmt, sess *session.Session) (*Result, error) {
	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	sch, err := e.loadSchema(ctx, txn, sess, s.Table)
	if err != nil {
		return nil, err
	}
	if _, idx := sch.findPolicy(s.PolicyName); idx >= 0 {
		return nil, newExecError("42710", "policy %q for table %q already exists", s.PolicyName, s.Table)
	}

	cmd := strings.ToUpper(strings.TrimSpace(s.Command))
	if cmd == "" {
		cmd = "ALL"
	}
	usingTxt, err := deparseOptExpr(s.Using)
	if err != nil {
		return nil, err
	}
	checkTxt, err := deparseOptExpr(s.WithCheck)
	if err != nil {
		return nil, err
	}
	sch.Policies = append(sch.Policies, rlsPolicy{
		Name:       s.PolicyName,
		Command:    cmd,
		Permissive: s.Permissive,
		Roles:      normalizeRoles(s.Roles),
		UsingExpr:  usingTxt,
		CheckExpr:  checkTxt,
	})
	if err := e.persistSchema(txn, sess, sch); err != nil {
		return nil, err
	}
	if err := e.commitTx(ctx, txn, owns); err != nil {
		return nil, err
	}
	return &Result{Command: "CREATE POLICY"}, nil
}

// execAlterPolicy modifies an existing policy's USING / WITH CHECK / roles
// (ALTER POLICY). Only the clauses present in the statement are changed.
func (e *execImpl) execAlterPolicy(ctx context.Context, s *ast.AlterPolicyStmt, sess *session.Session) (*Result, error) {
	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	sch, err := e.loadSchema(ctx, txn, sess, s.Table)
	if err != nil {
		return nil, err
	}
	p, idx := sch.findPolicy(s.PolicyName)
	if idx < 0 {
		return nil, newExecError("42704", "policy %q for table %q does not exist", s.PolicyName, s.Table)
	}
	if s.Roles != nil {
		p.Roles = normalizeRoles(s.Roles)
	}
	if s.Using != nil {
		txt, err := deparseOptExpr(s.Using)
		if err != nil {
			return nil, err
		}
		p.UsingExpr = txt
	}
	if s.WithCheck != nil {
		txt, err := deparseOptExpr(s.WithCheck)
		if err != nil {
			return nil, err
		}
		p.CheckExpr = txt
	}
	if err := e.persistSchema(txn, sess, sch); err != nil {
		return nil, err
	}
	if err := e.commitTx(ctx, txn, owns); err != nil {
		return nil, err
	}
	return &Result{Command: "ALTER POLICY"}, nil
}

// execDropPolicy removes a policy (DROP POLICY [IF EXISTS]).
func (e *execImpl) execDropPolicy(ctx context.Context, s *ast.DropPolicyStmt, sess *session.Session) (*Result, error) {
	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	sch, err := e.loadSchema(ctx, txn, sess, s.Table)
	if err != nil {
		// IF EXISTS on a missing table is a benign no-op.
		if s.IfExists {
			if execErr, ok := err.(*ExecError); ok && execErr.SQLSTATE == "42P01" {
				return &Result{Command: "DROP POLICY"}, nil
			}
		}
		return nil, err
	}
	_, idx := sch.findPolicy(s.PolicyName)
	if idx < 0 {
		if s.IfExists {
			return &Result{Command: "DROP POLICY"}, nil
		}
		return nil, newExecError("42704", "policy %q for table %q does not exist", s.PolicyName, s.Table)
	}
	sch.Policies = append(sch.Policies[:idx], sch.Policies[idx+1:]...)
	if err := e.persistSchema(txn, sess, sch); err != nil {
		return nil, err
	}
	if err := e.commitTx(ctx, txn, owns); err != nil {
		return nil, err
	}
	return &Result{Command: "DROP POLICY"}, nil
}

// deparseOptExpr deparses an optional policy predicate; nil yields "".
func deparseOptExpr(n ast.Node) (string, error) {
	if n == nil {
		return "", nil
	}
	return deparseExpr(n)
}

// normalizeRoles lowercases/trims role names and drops a sole PUBLIC (which is
// the same as "applies to everyone" / empty list). A nil/empty input stays nil.
func normalizeRoles(roles []string) []string {
	if len(roles) == 0 {
		return nil
	}
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		r = strings.TrimSpace(r)
		if r == "" || strings.EqualFold(r, "public") {
			// PUBLIC means everyone — represent as an empty list.
			return nil
		}
		out = append(out, r)
	}
	return out
}

// rlsApplies reports whether RLS enforcement is active for this session against
// the given table schema. It is active when the table has RLS enabled and the
// session does not bypass it. (The single-node engine has no per-table owner, so
// only a BYPASSRLS/admin session is exempt; FORCE is honored but only matters
// once per-owner exemption exists — kept here for forward-compatibility.)
func rlsApplies(sch *tableSchema, sess *session.Session) bool {
	if sch == nil || !sch.RLSEnabled {
		return false
	}
	if sess != nil && sess.IsBypassRLS() && !sch.RLSForced {
		return false
	}
	return true
}

// applyRLSSelect filters joined rows by the SELECT-command USING predicate of
// every RLS-enabled base table they reference. A boundRow survives only if every
// RLS-enabled binding in it passes. The predicate for a binding is evaluated
// with a resolver scoped to that binding's columns (so it references its own
// row), giving the per-table USING the same column scope CHECK constraints use.
func (e *execImpl) applyRLSSelect(sess *session.Session, rows []boundRow, params []Datum) ([]boundRow, error) {
	out := rows[:0]
	for _, row := range rows {
		keep := true
		for bi := range row {
			b := row[bi]
			if !rlsApplies(b.schema, sess) {
				continue
			}
			cells := b.cells
			resolve := rowResolver(b.schema, b.alias, cells)
			ok, err := e.rlsRowAllowed(b.schema, sess, "SELECT", params, resolve)
			if err != nil {
				return nil, err
			}
			if !ok {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, row)
		}
	}
	return out, nil
}

// rlsRowAllowed evaluates the combined USING predicate of the applicable
// policies for cmd against a row, using resolveCol to read the row's columns.
// Returns true when the row passes:
//
//	(OR of permissive USING) AND (AND of restrictive USING)
//
// with PostgreSQL default-deny: if no permissive policy applies, the row is
// denied. A policy whose USING is empty contributes TRUE (an unconditional
// permissive policy makes everything visible; an unconditional restrictive
// policy is a no-op AND TRUE).
func (e *execImpl) rlsRowAllowed(sch *tableSchema, sess *session.Session, cmd string, params []Datum, resolveCol func([]string) (value, error)) (bool, error) {
	user := ""
	if sess != nil {
		user = sess.User()
	}
	anyPermissive := false
	permissivePass := false
	for i := range sch.Policies {
		p := &sch.Policies[i]
		if !p.appliesToCommand(cmd) || !p.appliesToRole(user) {
			continue
		}
		if p.Permissive {
			anyPermissive = true
			ok, err := e.evalPolicyPredicate(p.UsingExpr, sess, params, resolveCol, true)
			if err != nil {
				return false, err
			}
			if ok {
				permissivePass = true
			}
		}
	}
	// Default-deny: RLS enabled, command applies, but no permissive policy passed.
	if !anyPermissive || !permissivePass {
		return false, nil
	}
	// All applicable restrictive policies must pass.
	for i := range sch.Policies {
		p := &sch.Policies[i]
		if p.Permissive || !p.appliesToCommand(cmd) || !p.appliesToRole(user) {
			continue
		}
		ok, err := e.evalPolicyPredicate(p.UsingExpr, sess, params, resolveCol, true)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// rlsCheckNewRow validates a NEW row (INSERT / UPDATE) against the WITH CHECK
// predicates of the applicable policies. PostgreSQL semantics: the new row must
// satisfy (OR of permissive WITH CHECK) AND (AND of restrictive WITH CHECK),
// with WITH CHECK defaulting to the policy's USING when omitted, and default-
// deny when no permissive policy applies. Returns nil when allowed, or a 42501
// ExecError when the row violates policy.
func (e *execImpl) rlsCheckNewRow(sch *tableSchema, sess *session.Session, cmd string, params []Datum, resolveCol func([]string) (value, error)) error {
	user := ""
	if sess != nil {
		user = sess.User()
	}
	anyPermissive := false
	permissivePass := false
	for i := range sch.Policies {
		p := &sch.Policies[i]
		if !p.appliesToCommand(cmd) || !p.appliesToRole(user) {
			continue
		}
		if p.Permissive {
			anyPermissive = true
			ok, err := e.evalPolicyPredicate(policyCheckExpr(p), sess, params, resolveCol, true)
			if err != nil {
				return err
			}
			if ok {
				permissivePass = true
			}
		}
	}
	if !anyPermissive || !permissivePass {
		return newExecError("42501", "new row violates row-level security policy for table %q", sch.Name)
	}
	for i := range sch.Policies {
		p := &sch.Policies[i]
		if p.Permissive || !p.appliesToCommand(cmd) || !p.appliesToRole(user) {
			continue
		}
		ok, err := e.evalPolicyPredicate(policyCheckExpr(p), sess, params, resolveCol, true)
		if err != nil {
			return err
		}
		if !ok {
			return newExecError("42501", "new row violates row-level security policy %q for table %q", p.Name, sch.Name)
		}
	}
	return nil
}

// permissiveText renders a policy's permissive flag as pg_policies does.
func permissiveText(permissive bool) string {
	if permissive {
		return "PERMISSIVE"
	}
	return "RESTRICTIVE"
}

// rolesArrayLiteral renders a policy's role list as the PostgreSQL text array
// literal pg_policies exposes: {public} when the policy applies to everyone.
func rolesArrayLiteral(roles []string) string {
	if len(roles) == 0 {
		return "{public}"
	}
	return "{" + strings.Join(roles, ",") + "}"
}

// optTextDatum renders an optional predicate text as a Datum (NULL when empty).
func optTextDatum(s string) Datum {
	if s == "" {
		return Datum{Null: true}
	}
	return Datum{Text: s}
}

// policyCheckExpr returns the predicate that validates a NEW row for a policy:
// its WITH CHECK if present, otherwise its USING (PostgreSQL fallback).
func policyCheckExpr(p *rlsPolicy) string {
	if p.CheckExpr != "" {
		return p.CheckExpr
	}
	return p.UsingExpr
}

// evalPolicyPredicate evaluates a single deparsed policy predicate against a row.
// An empty predicate is treated as the constant emptyDefault (TRUE for an
// unconditional policy). The predicate is evaluated with session access so it
// may reference current_setting(), current_user, etc. NULL collapses to false
// (PostgreSQL treats a NULL policy result as "not visible").
func (e *execImpl) evalPolicyPredicate(expr string, sess *session.Session, params []Datum, resolveCol func([]string) (value, error), emptyDefault bool) (bool, error) {
	if strings.TrimSpace(expr) == "" {
		return emptyDefault, nil
	}
	node, err := parseStoredExpr(expr)
	if err != nil {
		return false, err
	}
	ev := &evaluator{params: params, resolveCol: resolveCol, sess: sess}
	v, err := ev.eval(node)
	if err != nil {
		return false, err
	}
	if v.null {
		return false, nil
	}
	return asBool(v), nil
}
