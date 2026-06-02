package storage

// Stored-value tags. Every row value written through the engine is prefixed by
// one tag byte so the copy-on-write branch MergingIterator can distinguish a
// live row from a branch-local delete (tombstone) without decoding the tuple.
// (Gate 2 / a design decision.) These live in the storage package so both the
// executor (which encodes rows) and the branch package (which merges raw values
// during fall-through reads) reference one definition.
const (
	RowTagLive byte = 0x01
	RowTagTombstone byte = 0x00
)

// Tombstone is the stored value for a branch-local delete of a row that exists
// on the parent branch. It is a single tag byte.
func Tombstone() []byte { return []byte{RowTagTombstone} }

// IsTombstone reports whether a stored value is a COW delete marker.
func IsTombstone(b []byte) bool { return len(b) >= 1 && b[0] == RowTagTombstone }

// RawKeyForMerge constructs a Key from raw bytes. This is a narrow, documented
// escape used ONLY by the branch package's MERGE, which must re-target a branch
// data key to the equivalent parent-branch key by swapping the branch segment.
// No other package may use it; ordinary keys are produced by KeyEncoder.
func RawKeyForMerge(b []byte) Key { return Key{b: b} }
