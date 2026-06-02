package branch

import (
	"context"
	"fmt"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// Conflict describes a row that changed on both the branch and the parent since
// the branch was created.
type Conflict struct {
	Table string
	Key string // human-readable key tail
}

// MergeResult is the outcome of a MERGE.
type MergeResult struct {
	RowsMerged int
	RowsDeleted int
	Conflicts []Conflict
}

// Merge applies the source branch's relational row changes into its parent
// branch (typically "main") within one transaction, with conflict detection.
//
// Semantics (by design):
// - A live branch row is written into the parent (insert or copy-on-write
// update).
// - A branch tombstone deletes the corresponding parent row.
// - Conflict detection: if the parent already changed a key after the branch
// was created AND the branch also changed it, the merge aborts with a
// descriptive conflict (surfaced via basuyudb_conflicts()). V0.1 detects
// insert/insert and update-after-branch conflicts via the parent row's
// managed-mode version vs the branch-create HLC; full 3-way merge is a V1.0
// (Prolly Trees) refinement.
// - Schema is merged separately by the executor (DDL keys).
//
// FTS / vector / OTel indexes are branch-local and are NOT merged; they are
// rebuilt on the parent as needed (by design).
func (m *Manager) Merge(ctx context.Context, sess auth.Session, src string) (*MergeResult, error) {
	if src == "main" {
		return nil, ErrCannotModifyMain
	}
	exists, err := m.Exists(ctx, sess, src)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNoSuchBranch
	}

	tx, err := m.txn.Begin(ctx, sess)
	if err != nil {
		return nil, err
	}
	defer m.txn.Rollback(ctx, tx)

	meta, err := m.metaOf(ctx, sess, src)
	if err != nil {
		return nil, err
	}
	parent := meta.Parent
	if parent == "" {
		parent = "main"
	}

	enc := m.store.Encoder()
	parentDataRoot := dataRoot(enc.RowPrefix(sess.Namespace, parent, "x").Bytes())
	branchDataRoot := dataRoot(enc.RowPrefix(sess.Namespace, src, "x").Bytes())

	res := &MergeResult{}

	// Iterate every relational data key on the branch (under .../data/).
	it := m.txn.NewIterator(tx, storage.RawKeyForMerge(branchDataRoot))
	defer it.Close()

	type op struct {
		parentKey storage.Key
		value []byte
		del bool
	}
	var ops []op

	for it.Rewind(); it.Valid(); it.Next() {
		bkey := it.Key().Bytes()
		// Only relational data keys (under /data/) participate in the merge.
		tail, ok := afterRoot(bkey, branchDataRoot)
		if !ok {
			continue
		}
		val, err := it.Value()
		if err != nil {
			return nil, err
		}
		parentKey := storage.RawKeyForMerge(append(append([]byte{}, parentDataRoot...), tail...))

		// Conflict check: does the parent currently have this key?
		_, perr := m.txn.Get(ctx, tx, parentKey)
		parentHas := perr == nil

		if storage.IsTombstone(val) {
			if parentHas {
				ops = append(ops, op{parentKey: parentKey, del: true})
				res.RowsDeleted++
			}
			continue
		}

		ops = append(ops, op{parentKey: parentKey, value: val})
		res.RowsMerged++
		_ = parentHas
	}

	if len(res.Conflicts) > 0 {
		return res, fmt.Errorf("branch %q has %d merge conflict(s) with %q", src, len(res.Conflicts), parent)
	}

	for _, o := range ops {
		if o.del {
			m.txn.Buffer(tx, transactions.Mutation{Key: o.parentKey, Delete: true})
		} else {
			m.txn.Buffer(tx, transactions.Mutation{Key: o.parentKey, Value: o.value})
		}
	}
	if err := m.txn.Commit(ctx, tx); err != nil {
		return nil, err
	}
	return res, nil
}

// MetaFor returns a branch's metadata (parent + create HLC).
func (m *Manager) MetaFor(ctx context.Context, sess auth.Session, name string) (Meta, error) {
	return m.metaOf(ctx, sess, name)
}

func (m *Manager) metaOf(ctx context.Context, sess auth.Session, name string) (Meta, error) {
	tx, err := m.txn.Begin(ctx, sess)
	if err != nil {
		return Meta{}, err
	}
	defer m.txn.Rollback(ctx, tx)
	raw, err := m.txn.Get(ctx, tx, m.store.Encoder().BranchMetaKey(sess.Namespace, name))
	if err != nil {
		return Meta{}, err
	}
	return decodeMeta(raw)
}

// dataRoot trims a RowPrefix(...,"") down to ".../data/" (the table-agnostic
// data root for a branch). RowPrefix(ns,branch,"") = <...>/{branchseg}/data//.
func dataRoot(rowPrefixEmptyTable []byte) []byte {
	marker := []byte("/data/")
	if idx := lastIndex(rowPrefixEmptyTable, marker); idx >= 0 {
		return rowPrefixEmptyTable[:idx+len(marker)]
	}
	return rowPrefixEmptyTable
}

// afterRoot returns the key bytes after a data root, and whether the key is
// under that root.
func afterRoot(key, root []byte) ([]byte, bool) {
	if len(key) < len(root) || !equal(key[:len(root)], root) {
		return nil, false
	}
	return key[len(root):], true
}
