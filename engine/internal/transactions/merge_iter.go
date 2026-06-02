package transactions

import (
	"bytes"
	"sort"

	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
)

// bufferedForPrefix returns this transaction's buffered mutations whose key
// carries prefix, deduplicated last-write-wins and sorted ascending by key. It
// is the overlay merged onto the committed snapshot so a scan inside a
// transaction sees the transaction's own uncommitted writes (read-your-writes).
func bufferedForPrefix(muts []Mutation, prefix []byte) []Mutation {
	latest := map[string]Mutation{}
	for _, m := range muts {
		k := m.Key.Bytes()
		if bytes.HasPrefix(k, prefix) {
			latest[string(k)] = m // later mutations overwrite earlier
		}
	}
	if len(latest) == 0 {
		return nil
	}
	out := make([]Mutation, 0, len(latest))
	for _, m := range latest {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		return bytes.Compare(out[i].Key.Bytes(), out[j].Key.Bytes()) < 0
	})
	return out
}

// mergedIterator overlays a sorted buffer of this transaction's writes onto a
// committed-snapshot iterator. Forward only (Rewind/Seek/Next). Buffered entries
// override the snapshot at equal keys; buffered deletes hide the key.
type mergedIterator struct {
	store  storage.Iterator
	buf    []Mutation
	bi     int
	curKey storage.Key
	curVal []byte
	valid  bool
}

func (m *mergedIterator) Rewind() {
	m.store.Rewind()
	m.bi = 0
	m.produce()
}

func (m *mergedIterator) Seek(k storage.Key) {
	m.store.Seek(k)
	kb := k.Bytes()
	m.bi = sort.Search(len(m.buf), func(i int) bool {
		return bytes.Compare(m.buf[i].Key.Bytes(), kb) >= 0
	})
	m.produce()
}

func (m *mergedIterator) Next()              { m.produce() }
func (m *mergedIterator) Valid() bool        { return m.valid }
func (m *mergedIterator) Key() storage.Key   { return m.curKey }
func (m *mergedIterator) Value() ([]byte, error) {
	return m.curVal, nil
}
func (m *mergedIterator) Close() { m.store.Close() }

// produce advances to the next visible merged entry, skipping buffered deletes
// and snapshot entries overridden by the buffer.
func (m *mergedIterator) produce() {
	for {
		sValid := m.store.Valid()
		bValid := m.bi < len(m.buf)
		switch {
		case !sValid && !bValid:
			m.valid = false
			return
		case bValid && (!sValid || bytes.Compare(m.buf[m.bi].Key.Bytes(), m.store.Key().Bytes()) <= 0):
			b := m.buf[m.bi]
			// Snapshot at the same key is overridden by the buffered write.
			if sValid && bytes.Equal(b.Key.Bytes(), m.store.Key().Bytes()) {
				m.store.Next()
			}
			m.bi++
			if b.Delete {
				continue // tombstone hides the key
			}
			m.curKey = b.Key
			m.curVal = append([]byte(nil), b.Value...)
			m.valid = true
			return
		default:
			m.curKey = m.store.Key()
			v, err := m.store.Value()
			if err != nil {
				m.curVal = nil
			} else {
				m.curVal = v
			}
			m.store.Next()
			m.valid = true
			return
		}
	}
}
