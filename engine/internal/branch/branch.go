// Package branch implements BasuyuDB's copy-on-write branch-per-PR model
// (ADR-003, ADR-012, a design decision). A branch is created by writing a
// single metadata key (O(1), no data copy). Reads on a feature branch fall
// through to the parent ("main") for relational rows and secondary indexes via
// a MergingIterator; FTS/vector/OTel indexes are branch-local (no fall-through).
// MERGE applies the branch's row changes (and schema) into the parent within
// one transaction, with conflict detection.
//
// This package reads stored-value tombstone tags via storage.IsTombstone; it
// does not import the executor (cycle-free). The executor delegates branch reads
// to BranchStore.MergingIterator (by design).
package branch

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// Meta is the persisted branch metadata (one key per branch).
type Meta struct {
	Parent string
	CreatedAtHLC uint64
}

func encodeMeta(m Meta) []byte {
	b := make([]byte, 0, 16+len(m.Parent))
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], m.CreatedAtHLC)
	b = append(b, n[:]...)
	b = append(b, m.Parent...)
	return b
}

func decodeMeta(b []byte) (Meta, error) {
	if len(b) < 8 {
		return Meta{}, errors.New("branch: corrupt metadata")
	}
	return Meta{CreatedAtHLC: binary.BigEndian.Uint64(b[:8]), Parent: string(b[8:])}, nil
}

// Manager owns branch lifecycle and COW reads over a managed Store + the
// transaction engine.
type Manager struct {
	store storage.Store
	txn transactions.TransactionEngine
}

// NewManager constructs a branch Manager.
func NewManager(store storage.Store, txn transactions.TransactionEngine) *Manager {
	return &Manager{store: store, txn: txn}
}

// ErrBranchExists / ErrNoSuchBranch / ErrCannotModifyMain are branch errors.
var (
	ErrBranchExists = errors.New("branch already exists")
	ErrNoSuchBranch = errors.New("branch does not exist")
	ErrCannotModifyMain = errors.New("the main branch cannot be created, dropped, or merged into a branch")
)

// Exists reports whether a branch's metadata key is present (main always exists).
func (m *Manager) Exists(ctx context.Context, sess auth.Session, name string) (bool, error) {
	if name == "main" {
		return true, nil
	}
	tx, err := m.txn.Begin(ctx, sess)
	if err != nil {
		return false, err
	}
	defer m.txn.Rollback(ctx, tx)
	key := m.store.Encoder().BranchMetaKey(sess.Namespace, name)
	_, err = m.txn.Get(ctx, tx, key)
	if errors.Is(err, storage.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Create writes the branch metadata key (O(1) — no data is copied). This is the
// foundation of the <500ms CREATE BRANCH gate. (by design)
func (m *Manager) Create(ctx context.Context, sess auth.Session, name, from string) error {
	if name == "main" {
		return ErrCannotModifyMain
	}
	if from == "" {
		from = "main"
	}
	exists, err := m.Exists(ctx, sess, name)
	if err != nil {
		return err
	}
	if exists {
		return ErrBranchExists
	}

	tx, err := m.txn.Begin(ctx, sess)
	if err != nil {
		return err
	}
	defer m.txn.Rollback(ctx, tx)

	meta := Meta{Parent: from, CreatedAtHLC: m.txn.HLC().Now().Encode()}
	key := m.store.Encoder().BranchMetaKey(sess.Namespace, name)
	m.txn.Buffer(tx, transactions.Mutation{Key: key, Value: encodeMeta(meta)})
	return m.txn.Commit(ctx, tx)
}

// Drop removes all keys under the branch prefix and its metadata. main is
// protected.
func (m *Manager) Drop(ctx context.Context, sess auth.Session, name string) error {
	if name == "main" {
		return ErrCannotModifyMain
	}
	exists, err := m.Exists(ctx, sess, name)
	if err != nil {
		return err
	}
	if !exists {
		return ErrNoSuchBranch
	}

	// Drop the whole branch data subtree via the underlying badger DropPrefix.
	db, ok := m.store.BadgerDB().(interface{ DropPrefix(...[]byte) error })
	if !ok {
		return errors.New("branch: store does not support prefix drop")
	}
	branchData := branchSubtreePrefix(m.store, sess, name)
	if err := db.DropPrefix(branchData); err != nil {
		return fmt.Errorf("branch: drop subtree: %w", err)
	}

	// Delete the metadata key.
	tx, err := m.txn.Begin(ctx, sess)
	if err != nil {
		return err
	}
	defer m.txn.Rollback(ctx, tx)
	m.txn.Buffer(tx, transactions.Mutation{Key: m.store.Encoder().BranchMetaKey(sess.Namespace, name), Delete: true})
	return m.txn.Commit(ctx, tx)
}

// branchSubtreePrefix returns the /ns/{ns}/br/{name}/ raw prefix for DropPrefix.
// It is derived by truncating a RowPrefix to the branch root (before /data/).
func branchSubtreePrefix(store storage.Store, sess auth.Session, name string) []byte {
	full := store.Encoder().RowPrefix(sess.Namespace, name, "x").Bytes()
	// RowPrefix = <ver>/ns/{ns}/br/{name}/data/x/ ; trim "data/x/" (7+... ) by
	// cutting at "/data/". Find the "/data/" marker and keep up to it + 1.
	marker := []byte("/data/")
	if idx := lastIndex(full, marker); idx >= 0 {
		return full[:idx+1] // include trailing slash before "data"
	}
	return full
}

func lastIndex(b, sub []byte) int {
	for i := len(b) - len(sub); i >= 0; i-- {
		if equal(b[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

func equal(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
