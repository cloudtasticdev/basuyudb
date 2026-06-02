// Package storage owns BadgerDB, the opaque Key type, the KeyEncoder, and the
// Store interface. It is the ONLY package that encodes keys or performs IO.
// BadgerDB is opened in MANAGED MODE (managedTxns=true). (by design;
// the design specs §3, §4, §9.)
//
// Conformed to the design specs (the reconciliation pass). Resolves the architecture review (one key
// encoder), the architecture review (raw namespace), the architecture review (sibling main), (one branch-meta
// key), the integration review (OtelSpanKey replaces keyspace.Prefix).
package storage

import (
	"encoding/binary"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
)

// KeyVersion is the leading version byte on every key.
const KeyVersion byte = 0x01

// Key is an opaque, validated BadgerDB key. It cannot be constructed outside
// this package: the only sources of a Key are KeyEncoder methods. This gives a
// compile-time guarantee that every key in the system passes through the one
// canonical encoder. The CI lint forbids any "/ns/" string literal outside
// this package. (by design)
type Key struct {
	b []byte // unexported: a Key cannot be forged
}

// Bytes returns the raw key bytes for IO. It is the only way to read a Key's
// content; callers must not mutate the returned slice.
func (k Key) Bytes() []byte { return k.b }

// rawKey constructs a Key from already-encoded bytes. Package-private so that
// only KeyEncoder methods (and badger read-back into Key) may produce a Key.
func rawKey(b []byte) Key { return Key{b: b} }

// IntentSuffix is the co-located Percolator intent discriminator.
type IntentSuffix byte

const (
	IntentWrite IntentSuffix = 'W'
	IntentValue IntentSuffix = 'V'
	IntentLock IntentSuffix = 'L'
)

// KeyEncoder constructs every Key family. The namespace carried is the RAW
// validated namespace string from auth.Session (NOT hashed). Main is a sibling
// path; feature branches live under /br/. (by design)
type KeyEncoder interface {
	RowKey(ns auth.NamespaceID, branch, table string, pk []byte) Key
	RowPrefix(ns auth.NamespaceID, branch, table string) Key
	IndexKey(ns auth.NamespaceID, branch, table, col string, val, pk []byte) Key
	FTSKey(ns auth.NamespaceID, branch, table, field, term string, docID []byte) Key
	VectorKey(ns auth.NamespaceID, branch, table, col string, id []byte) Key
	OtelSpanKey(ns auth.NamespaceID, branch string, traceID, spanID []byte) Key
	OtelIndexKey(ns auth.NamespaceID, branch, indexName string, vals ...[]byte) Key
	SchemaKey(ns auth.NamespaceID, table string) Key
	BranchMetaKey(ns auth.NamespaceID, name string) Key
	RaftKey(ns auth.NamespaceID, groupID uint64, suffix []byte) Key
	HLCKey(ns auth.NamespaceID) Key
	IntentKey(row Key, hlc uint64, suffix IntentSuffix) Key
	NamespacePrefix(ns auth.NamespaceID) Key
}

// keyEncoder is the canonical KeyEncoder implementation.
//
// Key wire format (by design):
//
//	[0x01] <structural string path> [length-prefixed binary tail...]
//
// String path segments are written verbatim ("/ns/{ns}/main/data/{table}/").
// Binary tail components (pk, val, id, docID) are length-prefixed (4-byte
// big-endian length + bytes) so they are unambiguous even when they contain
// '/' or other path characters. A RowPrefix is exactly the structural string
// path with no binary tail, so a prefix scan over RowPrefix matches all RowKeys
// for that table.
type keyEncoder struct{}

// branchSegment returns the canonical branch path component. main is a SIBLING
// ("/main"), not a "/br/main" child. (by design)
func branchSegment(branch string) string {
	if branch == "main" || branch == "" {
		return "/main"
	}
	return "/br/" + branch
}

// nsRoot returns the version byte + "/ns/{ns}" structural prefix.
func nsRoot(ns auth.NamespaceID) []byte {
	b := make([]byte, 0, 8+len(ns.String())+16)
	b = append(b, KeyVersion)
	b = append(b, "/ns/"...)
	b = append(b, ns.String()...)
	return b
}

// appendStr appends "/" + s.
func appendStr(b []byte, s string) []byte {
	b = append(b, '/')
	return append(b, s...)
}

// appendBin appends a length-prefixed binary component.
func appendBin(b, bin []byte) []byte {
	var lp [4]byte
	binary.BigEndian.PutUint32(lp[:], uint32(len(bin)))
	b = append(b, '/')
	b = append(b, lp[:]...)
	return append(b, bin...)
}

func (keyEncoder) RowPrefix(ns auth.NamespaceID, branch, table string) Key {
	b := nsRoot(ns)
	b = append(b, branchSegment(branch)...)
	b = appendStr(b, "data")
	b = appendStr(b, table)
	b = append(b, '/') // trailing separator so prefix matches RowKey tail
	return rawKey(b)
}

func (e keyEncoder) RowKey(ns auth.NamespaceID, branch, table string, pk []byte) Key {
	// /ns/{ns}/{main|br/branch}/data/{table}/<lp:pk>
	b := nsRoot(ns)
	b = append(b, branchSegment(branch)...)
	b = appendStr(b, "data")
	b = appendStr(b, table)
	b = appendBin(b, pk)
	return rawKey(b)
}

func (keyEncoder) IndexKey(ns auth.NamespaceID, branch, table, col string, val, pk []byte) Key {
	// /ns/{ns}/{main|br/branch}/idx/{table}/{col}/<lp:val>/<lp:pk>
	b := nsRoot(ns)
	b = append(b, branchSegment(branch)...)
	b = appendStr(b, "idx")
	b = appendStr(b, table)
	b = appendStr(b, col)
	b = appendBin(b, val)
	b = appendBin(b, pk)
	return rawKey(b)
}

func (keyEncoder) FTSKey(ns auth.NamespaceID, branch, table, field, term string, docID []byte) Key {
	// /ns/{ns}/br/{branch}/fts/{table}/{field}/{term}/<lp:docID> (branch-local)
	b := nsRoot(ns)
	b = append(b, branchSegment(branch)...)
	b = appendStr(b, "fts")
	b = appendStr(b, table)
	b = appendStr(b, field)
	b = appendStr(b, term)
	b = appendBin(b, docID)
	return rawKey(b)
}

func (keyEncoder) VectorKey(ns auth.NamespaceID, branch, table, col string, id []byte) Key {
	// /ns/{ns}/br/{branch}/vec/{table}/{col}/<lp:id> (branch-local)
	b := nsRoot(ns)
	b = append(b, branchSegment(branch)...)
	b = appendStr(b, "vec")
	b = appendStr(b, table)
	b = appendStr(b, col)
	b = appendBin(b, id)
	return rawKey(b)
}

func (keyEncoder) OtelSpanKey(ns auth.NamespaceID, branch string, traceID, spanID []byte) Key {
	// /ns/{ns}/{branchpath}/data/otel_spans/<lp:traceID>/<lp:spanID> (branch-local)
	b := nsRoot(ns)
	b = append(b, branchSegment(branch)...)
	b = appendStr(b, "data")
	b = appendStr(b, "otel_spans")
	b = appendBin(b, traceID)
	b = appendBin(b, spanID)
	return rawKey(b)
}

func (keyEncoder) OtelIndexKey(ns auth.NamespaceID, branch, indexName string, vals ...[]byte) Key {
	// /ns/{ns}/{branchpath}/idx/otel_spans/{indexName}/<lp:val>...
	b := nsRoot(ns)
	b = append(b, branchSegment(branch)...)
	b = appendStr(b, "idx")
	b = appendStr(b, "otel_spans")
	b = appendStr(b, indexName)
	for _, v := range vals {
		b = appendBin(b, v)
	}
	return rawKey(b)
}

func (keyEncoder) SchemaKey(ns auth.NamespaceID, table string) Key {
	// /ns/{ns}/meta/schema/{table}
	b := nsRoot(ns)
	b = appendStr(b, "meta")
	b = appendStr(b, "schema")
	b = appendStr(b, table)
	return rawKey(b)
}

func (keyEncoder) BranchMetaKey(ns auth.NamespaceID, name string) Key {
	// /ns/{ns}/meta/branches/{name} (by design)
	b := nsRoot(ns)
	b = appendStr(b, "meta")
	b = appendStr(b, "branches")
	b = appendStr(b, name)
	return rawKey(b)
}

func (keyEncoder) RaftKey(ns auth.NamespaceID, groupID uint64, suffix []byte) Key {
	// /ns/{ns}/raft/{groupID}/<suffix>
	b := nsRoot(ns)
	b = appendStr(b, "raft")
	var g [8]byte
	binary.BigEndian.PutUint64(g[:], groupID)
	b = appendBin(b, g[:])
	b = appendBin(b, suffix)
	return rawKey(b)
}

func (keyEncoder) HLCKey(ns auth.NamespaceID) Key {
	// /ns/{ns}/hlc/ts
	b := nsRoot(ns)
	b = appendStr(b, "hlc")
	b = appendStr(b, "ts")
	return rawKey(b)
}

func (keyEncoder) IntentKey(row Key, hlc uint64, suffix IntentSuffix) Key {
	// {RowKey}@{hlc}:{suffix} co-located with the row (by design)
	src := row.Bytes()
	b := make([]byte, 0, len(src)+11)
	b = append(b, src...)
	b = append(b, '@')
	var h [8]byte
	binary.BigEndian.PutUint64(h[:], hlc)
	b = append(b, h[:]...)
	b = append(b, ':')
	b = append(b, byte(suffix))
	return rawKey(b)
}

func (keyEncoder) NamespacePrefix(ns auth.NamespaceID) Key {
	// /ns/{ns}/ (GDPR erasure prefix)
	b := nsRoot(ns)
	b = append(b, '/')
	return rawKey(b)
}
