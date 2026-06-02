package executor

import (
	"encoding/binary"
	"math"
	"strconv"
	"strings"
)

// orderEncode turns a column value (PostgreSQL text form) into a memcomparable
// byte string: bytewise order of the result equals the logical order of the
// values for that column type (ADR-022). This is what lets a BadgerDB prefix
// scan over a secondary index return rows in value order, enabling range scans
// and ORDER BY.
//
// The encoding is self-delimiting per type (fixed width, or 0x00 0x00-terminated
// for text), so a row's pk can follow it in the index key without disturbing
// value ordering. NULLs are never indexed (callers skip them), matching SQL
// semantics for equality, ranges, and ORDER BY.
func orderEncode(typeOID uint32, text string) ([]byte, error) {
	switch typeOID {
	case OIDInt4, OIDInt8:
		v, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err != nil {
			return nil, newExecError("22P02", "invalid integer for index: %q", text)
		}
		// Flip the sign bit so negatives (which have it set) sort before
		// positives under unsigned bytewise comparison.
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(v)^(1<<63))
		return b[:], nil

	case OIDFloat8:
		f, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil {
			return nil, newExecError("22P02", "invalid float for index: %q", text)
		}
		bits := math.Float64bits(f)
		// Total-order transform: if the sign bit is set (negative) flip all
		// bits; otherwise flip only the sign bit. Yields ascending IEEE-754 order.
		if bits&(1<<63) != 0 {
			bits = ^bits
		} else {
			bits |= 1 << 63
		}
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], bits)
		return b[:], nil

	case OIDBool:
		if isTruthy(text) {
			return []byte{0x01}, nil
		}
		return []byte{0x00}, nil

	default: // OIDText and anything else: lexicographic on the raw bytes.
		return encodeOrderedText(text), nil
	}
}

// encodeOrderedText escapes 0x00 as 0x00 0xFF and terminates with 0x00 0x00, so
// that "ab" < "abc" and the encoding is prefix-free (no value's encoding is a
// prefix of another's — the terminator can't appear inside an escaped value).
func encodeOrderedText(s string) []byte {
	out := make([]byte, 0, len(s)+2)
	for i := 0; i < len(s); i++ {
		if s[i] == 0x00 {
			out = append(out, 0x00, 0xFF)
		} else {
			out = append(out, s[i])
		}
	}
	return append(out, 0x00, 0x00)
}

func isTruthy(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "t", "true", "1", "y", "yes", "on":
		return true
	}
	return false
}
