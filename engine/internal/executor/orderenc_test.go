package executor

import (
	"bytes"
	"testing"
)

// assertOrdered checks that orderEncode preserves the given logical order:
// for inputs already sorted logically, the encodings must be sorted bytewise.
func assertOrdered(t *testing.T, oid uint32, vals []string) {
	t.Helper()
	var prev []byte
	for i, v := range vals {
		enc, err := orderEncode(oid, v)
		if err != nil {
			t.Fatalf("encode %q: %v", v, err)
		}
		if i > 0 && bytes.Compare(prev, enc) >= 0 {
			t.Fatalf("order broken at %q (oid=%d): enc(%v) !< enc(%q)", v, oid, vals[i-1], v)
		}
		prev = enc
	}
}

func TestOrderEncodeInt(t *testing.T) {
	// Logical ascending order including negatives and zero crossing.
	assertOrdered(t, OIDInt8, []string{"-9223372036854775808", "-1000", "-1", "0", "1", "42", "1000", "9223372036854775807"})
	// Critical: lexicographic text would put "10" < "9"; memcomparable must not.
	assertOrdered(t, OIDInt4, []string{"2", "9", "10", "100"})
}

func TestOrderEncodeFloat(t *testing.T) {
	assertOrdered(t, OIDFloat8, []string{"-1e10", "-3.5", "-0.001", "0", "0.001", "3.5", "1e10"})
}

func TestOrderEncodeText(t *testing.T) {
	assertOrdered(t, OIDText, []string{"", "a", "ab", "abc", "b", "ba", "z"})
	// Prefix-free: "ab" must sort before "ab\x00..." style neighbours.
	assertOrdered(t, OIDText, []string{"ab", "ab\x00", "aba"})
}

func TestOrderEncodeBool(t *testing.T) {
	f, _ := orderEncode(OIDBool, "f")
	tr, _ := orderEncode(OIDBool, "t")
	if bytes.Compare(f, tr) >= 0 {
		t.Fatalf("false must sort before true")
	}
}

func TestOrderEncodeBadNumber(t *testing.T) {
	if _, err := orderEncode(OIDInt8, "not-a-number"); err == nil {
		t.Fatal("expected error for non-numeric int value")
	}
}
