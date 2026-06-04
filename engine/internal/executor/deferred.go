package executor

import (
	"context"
	"sync"

	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// deferredRegistry maps each explicit *transactions.Txn to its DEFERRABLE
// constraint state. It is a thin guarded map so the transactions package needs
// no awareness of deferred checking. The zero value is ready to use.
type deferredRegistry struct {
	mu sync.Mutex
	m  map[*transactions.Txn]*deferredState
}

// deferredState holds, for one explicit transaction, the SET CONSTRAINTS
// per-constraint overrides and the queue of FK checks deferred to COMMIT.
type deferredState struct {
	// overrides maps a constraint name to its current deferred mode as set by
	// SET CONSTRAINTS name DEFERRED/IMMEDIATE. allOverride/allDeferred capture a
	// SET CONSTRAINTS ALL DEFERRED/IMMEDIATE that applies to every constraint not
	// named individually afterwards.
	overrides   map[string]bool
	allOverride bool
	allDeferred bool

	// pending is the ordered list of deferred FK checks to run at COMMIT (or
	// when SET CONSTRAINTS ... IMMEDIATE re-checks a constraint).
	pending []pendingFKCheck
}

// pendingFKKind distinguishes the two deferrable FK verification checks.
type pendingFKKind int

const (
	// pendingFKChild: a child INSERT/UPDATE row must reference an existing parent
	// tuple (the child-side existence check).
	pendingFKChild pendingFKKind = iota
	// pendingFKParentRestrict: a parent row was deleted or its referenced key
	// changed under RESTRICT/NO ACTION and must leave no orphaned child row.
	pendingFKParentRestrict
)

// pendingFKCheck captures enough state to re-run one FK verification against the
// transaction's final committed snapshot. For pendingFKChild it stores the
// child FK column values and the parent's referenced columns. For
// pendingFKParentRestrict it stores the parent table and the referenced key
// values whose child rows must not exist.
type pendingFKCheck struct {
	kind       pendingFKKind
	constraint string // FK constraint name (for SET CONSTRAINTS matching)

	// Child-side existence check (pendingFKChild).
	childTable string
	parentTab  string
	refCols    []string
	vals       []string

	// Parent-side RESTRICT check (pendingFKParentRestrict).
	childTabFilter string // only this child table's matching rows must be absent
	fkAnchor       int    // anchor column index of the child FK
}

// stateFor returns the deferred state for tx, creating it on first use. tx must
// be non-nil (autocommit never defers).
func (r *deferredRegistry) stateFor(tx *transactions.Txn) *deferredState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = make(map[*transactions.Txn]*deferredState)
	}
	st := r.m[tx]
	if st == nil {
		st = &deferredState{overrides: make(map[string]bool)}
		r.m[tx] = st
	}
	return st
}

// peek returns the existing state for tx without creating one (nil if none).
func (r *deferredRegistry) peek(tx *transactions.Txn) *deferredState {
	if tx == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[tx]
}

// clear drops all deferred state for tx (called on COMMIT and ROLLBACK).
func (r *deferredRegistry) clear(tx *transactions.Txn) {
	if tx == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, tx)
}

// isDeferred reports the effective deferral for a constraint in this txn: a
// later SET CONSTRAINTS override (by name, else ALL) wins over the constraint's
// INITIALLY DEFERRED default.
func (st *deferredState) isDeferred(name string, initiallyDeferred bool) bool {
	if st == nil {
		return initiallyDeferred
	}
	if v, ok := st.overrides[name]; ok {
		return v
	}
	if st.allOverride {
		return st.allDeferred
	}
	return initiallyDeferred
}

// setMode applies SET CONSTRAINTS to the state. all=true sets the blanket mode
// and clears per-name overrides; otherwise it records each named override.
func (st *deferredState) setMode(all bool, names []string, deferred bool) {
	if all {
		st.allOverride = true
		st.allDeferred = deferred
		st.overrides = make(map[string]bool)
		return
	}
	for _, n := range names {
		st.overrides[n] = deferred
	}
}

// record appends a deferred FK check.
func (st *deferredState) record(c pendingFKCheck) {
	st.pending = append(st.pending, c)
}

// effectiveDeferral reports whether an FK is deferred in ctx's transaction.
// Deferral requires (a) an explicit transaction, (b) a DEFERRABLE constraint,
// and (c) the effective mode (SET CONSTRAINTS override, else INITIALLY DEFERRED)
// being deferred. In autocommit there is no txn, so it always returns false.
func (e *execImpl) effectiveDeferral(ctx context.Context, fk fkConstraint) bool {
	if !fk.deferrable {
		return false
	}
	tx := txnFromCtx(ctx)
	if tx == nil {
		return false
	}
	return e.deferred.peek(tx).isDeferred(fk.name, fk.initiallyDeferred)
}

// runPendingChecks runs the txn's deferred FK checks, returning the first
// violation. When names is non-nil it runs only checks for the named
// constraints (used by SET CONSTRAINTS ... IMMEDIATE) and removes the ones it
// ran; when names is nil it runs all of them (used at COMMIT). matched reports
// whether any check was run when names is non-nil.
func (e *execImpl) runPendingChecks(ctx context.Context, txn *transactions.Txn, sess *session.Session, st *deferredState, names map[string]bool) error {
	if st == nil {
		return nil
	}
	remaining := st.pending[:0:0]
	for _, p := range st.pending {
		if names != nil && !names[p.constraint] {
			remaining = append(remaining, p)
			continue
		}
		if err := e.runOnePending(ctx, txn, sess, p); err != nil {
			// Keep the unprocessed tail so a re-check can run them later.
			st.pending = append(remaining, p)
			return err
		}
	}
	if names != nil {
		st.pending = remaining
	} else {
		st.pending = nil
	}
	return nil
}

// runOnePending re-evaluates a single deferred FK check against the txn's
// current (final, at COMMIT) snapshot.
func (e *execImpl) runOnePending(ctx context.Context, txn *transactions.Txn, sess *session.Session, p pendingFKCheck) error {
	switch p.kind {
	case pendingFKChild:
		ok, err := e.parentTupleExists(ctx, txn, sess, p.parentTab, p.refCols, p.vals)
		if err != nil {
			return err
		}
		if !ok {
			return newExecError("23503",
				"insert or update on table %q violates foreign key constraint %q: key is not present in table %q",
				p.childTable, p.constraint, p.parentTab)
		}
	case pendingFKParentRestrict:
		orphan, err := e.deferredParentOrphanExists(ctx, txn, sess, p)
		if err != nil {
			return err
		}
		if orphan {
			return newExecError("23503",
				"update or delete on table %q violates foreign key constraint %q on table %q",
				p.parentTab, p.constraint, p.childTabFilter)
		}
	}
	return nil
}

// deferredParentOrphanExists reports whether any child row in the recorded child
// table still references the parent key values that were deleted/changed (i.e. a
// RESTRICT violation persists in the final snapshot).
func (e *execImpl) deferredParentOrphanExists(ctx context.Context, txn *transactions.Txn, sess *session.Session, p pendingFKCheck) (bool, error) {
	csch, err := e.loadSchema(ctx, txn, sess, p.childTabFilter)
	if err != nil {
		return false, err
	}
	// Locate the FK on the child schema by name so a schema change cannot
	// misalign the anchor index.
	var fk fkConstraint
	found := false
	for _, c := range childForeignKeys(csch) {
		if c.name == p.constraint {
			fk = c
			found = true
			break
		}
	}
	if !found {
		return false, nil // constraint dropped: nothing to enforce
	}
	sc, err := e.scanTable(ctx, txn, sess, p.childTabFilter, p.childTabFilter)
	if err != nil {
		return false, err
	}
	var matched bool
	for _, r := range sc.rows {
		if childTupleMatches(csch, r.cells, fk, p.vals) {
			matched = true
			break
		}
	}
	if !matched {
		// No child still references the old key: the cascade/delete is fine.
		return false, nil
	}
	// NO ACTION semantics: a child still references the key — only a violation
	// if no parent row provides that key in the final snapshot (e.g. the key was
	// re-inserted into the parent within the same transaction).
	refCols := make([]string, len(fk.pairs))
	for k, pr := range fk.pairs {
		refCols[k] = pr.Ref
	}
	parentOK, err := e.parentTupleExists(ctx, txn, sess, p.parentTab, refCols, p.vals)
	if err != nil {
		return false, err
	}
	return !parentOK, nil
}

// SetConstraints implements the Executor interface (SET CONSTRAINTS).
func (e *execImpl) SetConstraints(ctx context.Context, tx *transactions.Txn, sess *session.Session, all bool, names []string, deferred bool) error {
	// Outside an explicit transaction there is nothing to defer (the surrounding
	// statement autocommits), so SET CONSTRAINTS is an accepted no-op.
	if tx == nil {
		return nil
	}
	st := e.deferred.stateFor(tx)
	st.setMode(all, names, deferred)
	if deferred {
		return nil
	}
	// IMMEDIATE: re-check the now-immediate constraints' pending violations.
	var filter map[string]bool
	if !all {
		filter = make(map[string]bool, len(names))
		for _, n := range names {
			filter[n] = true
		}
	} else {
		// ALL IMMEDIATE: run every pending check (names==nil runs all).
	}
	return e.runPendingChecks(ctx, tx, sess, st, filter)
}
