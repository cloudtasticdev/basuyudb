package executor

import (
	"encoding/binary"

	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
)

// Row encoding: a stored value is [1-byte tag][tuple]. The tag distinguishes a
// live row from a copy-on-write tombstone. A live row's tuple is an ordered
// sequence of cells matching the table's column order; each cell is
// [1-byte null flag][uint32 len][bytes] in PG text format. The tombstone
// (rowTagTombstone) marks a row deleted on a feature branch so the branch
// MergingIterator suppresses fall-through to main (Gate 2 / a design decision).
// Row tags live in the storage package (storage.RowTagLive / RowTagTombstone)
// so the branch MergingIterator can read them without importing the executor.

// encodeRow serialises an ordered slice of cells into the stored tuple format,
// prefixed by the live-row tag.
func encodeRow(cells []Datum) []byte {
	out := make([]byte, 0, 16*len(cells)+1)
	out = append(out, storage.RowTagLive)
	var lp [4]byte
	for _, c := range cells {
		if c.Null {
			out = append(out, 1) // null flag set
			binary.BigEndian.PutUint32(lp[:], 0)
			out = append(out, lp[:]...)
			continue
		}
		out = append(out, 0) // not null
		binary.BigEndian.PutUint32(lp[:], uint32(len(c.Text)))
		out = append(out, lp[:]...)
		out = append(out, c.Text...)
	}
	return out
}

// decodeRow deserialises a stored tuple into n cells. n must equal the table's
// column count at read time. The leading tag byte is consumed first; callers
// must not pass a tombstone value (IsTombstone) to decodeRow.
func decodeRow(b []byte, n int) ([]Datum, error) {
	if len(b) < 1 {
		return nil, newExecError("XX000", "corrupt row: empty value")
	}
	if b[0] == storage.RowTagTombstone {
		return nil, newExecError("XX000", "attempt to decode a tombstone row")
	}
	cells := make([]Datum, 0, n)
	pos := 1 // skip the live-row tag
	for i := 0; i < n; i++ {
		if pos+5 > len(b) {
			return nil, newExecError("XX000", "corrupt row tuple: truncated header for column %d", i)
		}
		null := b[pos] == 1
		pos++
		l := int(binary.BigEndian.Uint32(b[pos:]))
		pos += 4
		if pos+l > len(b) {
			return nil, newExecError("XX000", "corrupt row tuple: truncated value for column %d", i)
		}
		if null {
			cells = append(cells, Datum{Null: true})
		} else {
			cells = append(cells, Datum{Text: string(b[pos : pos+l])})
		}
		pos += l
	}
	return cells, nil
}
