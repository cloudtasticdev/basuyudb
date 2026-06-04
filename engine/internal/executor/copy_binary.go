package executor

import (
	"encoding/binary"
	"encoding/hex"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// PGCOPY binary format (COPY ... WITH (FORMAT binary)).
//
// Stream layout:
//   - 11-byte signature: "PGCOPY\n\377\r\n\0"
//   - int32 flags field (0)
//   - int32 header-extension-length (0)
//   - per row: int16 field count, then for each field: int32 length + that many
//     bytes of binary payload; length == -1 means SQL NULL (no bytes follow).
//   - trailer: int16 == -1 as the field count of a final sentinel row.
//
// Per-field payloads use PostgreSQL's binary wire format keyed by the column's
// type OID (res.Columns carries the OIDs). The engine stores cells as PG text,
// so encode converts text→binary and decode converts binary→text, round-tripping
// through the same text representation the rest of the executor uses.

// pgCopySignature is the fixed 11-byte PGCOPY magic.
var pgCopySignature = []byte{'P', 'G', 'C', 'O', 'P', 'Y', '\n', 0xFF, '\r', '\n', 0x00}

// copyBinaryEpoch is PostgreSQL's date/timestamp origin: 2000-01-01 00:00:00 UTC.
var copyBinaryEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

var copyTSLayouts = []string{
	time.RFC3339Nano, time.RFC3339,
	"2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05", "2006-01-02",
}

func copyParseTime(text string) (time.Time, bool) {
	for _, l := range copyTSLayouts {
		if t, err := time.Parse(l, strings.TrimSpace(text)); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// formatCopyBinary renders the complete PGCOPY binary stream for a result.
func formatCopyBinary(res *Result) []byte {
	var b []byte
	b = append(b, pgCopySignature...)
	b = appendInt32(b, 0) // flags
	b = appendInt32(b, 0) // header extension length

	ncols := len(res.Columns)
	for _, row := range res.Rows {
		// Field count is the number of columns in the result.
		b = appendInt16(b, int16(len(row)))
		for i, d := range row {
			if d.Null {
				b = appendInt32(b, -1)
				continue
			}
			oid := OIDText
			if i < ncols {
				oid = res.Columns[i].TypeOID
			}
			payload := encodeCopyBinaryField(d.Text, oid)
			b = appendInt32(b, int32(len(payload)))
			b = append(b, payload...)
		}
	}
	// Trailer: a row whose field count is -1.
	b = appendInt16(b, -1)
	return b
}

// parseCopyBinary decodes a PGCOPY binary stream into text-format datums. oids
// (len ncols) drives per-field binary→text conversion; a short/empty oids slice
// falls back to text for the missing positions.
func parseCopyBinary(data []byte, oids []uint32) ([][]Datum, error) {
	pos := 0
	if len(data) < len(pgCopySignature)+8 {
		return nil, newExecError("22P03", "COPY binary stream too short for header")
	}
	for i := range pgCopySignature {
		if data[pos+i] != pgCopySignature[i] {
			return nil, newExecError("22P03", "COPY binary signature mismatch")
		}
	}
	pos += len(pgCopySignature)
	// flags (int32)
	_ = int32(binary.BigEndian.Uint32(data[pos:]))
	pos += 4
	// header extension length (int32) — skip that many bytes.
	extLen := int(int32(binary.BigEndian.Uint32(data[pos:])))
	pos += 4
	if extLen < 0 || pos+extLen > len(data) {
		return nil, newExecError("22P03", "COPY binary header extension out of range")
	}
	pos += extLen

	var rows [][]Datum
	for {
		if pos+2 > len(data) {
			return nil, newExecError("22P03", "COPY binary truncated before field count")
		}
		nfields := int(int16(binary.BigEndian.Uint16(data[pos:])))
		pos += 2
		if nfields == -1 {
			break // trailer
		}
		if nfields < 0 {
			return nil, newExecError("22P03", "COPY binary invalid field count %d", nfields)
		}
		row := make([]Datum, nfields)
		for f := 0; f < nfields; f++ {
			if pos+4 > len(data) {
				return nil, newExecError("22P03", "COPY binary truncated before field length")
			}
			flen := int(int32(binary.BigEndian.Uint32(data[pos:])))
			pos += 4
			if flen == -1 {
				row[f] = Datum{Null: true}
				continue
			}
			if flen < 0 || pos+flen > len(data) {
				return nil, newExecError("22P03", "COPY binary field length %d out of range", flen)
			}
			oid := OIDText
			if f < len(oids) {
				oid = oids[f]
			}
			text, err := decodeCopyBinaryField(data[pos:pos+flen], oid)
			if err != nil {
				return nil, err
			}
			row[f] = Datum{Text: text}
			pos += flen
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// encodeCopyBinaryField converts a PG text value to its binary wire form for the
// given type OID. Unknown / variable-length textual types fall through to UTF-8
// bytes (matching PG for text/varchar/json and the engine's wire encoder).
func encodeCopyBinaryField(text string, oid uint32) []byte {
	switch oid {
	case OIDInt2:
		n, _ := strconv.ParseInt(text, 10, 64)
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(int16(n)))
		return b[:]
	case OIDInt4:
		n, _ := strconv.ParseInt(text, 10, 64)
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(int32(n)))
		return b[:]
	case OIDOid:
		n, _ := strconv.ParseUint(text, 10, 32)
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(n))
		return b[:]
	case OIDInt8:
		n, _ := strconv.ParseInt(text, 10, 64)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		return b[:]
	case OIDFloat4:
		f, _ := strconv.ParseFloat(text, 32)
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], math.Float32bits(float32(f)))
		return b[:]
	case OIDFloat8:
		f, _ := strconv.ParseFloat(text, 64)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], math.Float64bits(f))
		return b[:]
	case OIDBool:
		if text == "t" || text == "true" || text == "1" || text == "T" {
			return []byte{1}
		}
		return []byte{0}
	case OIDChar:
		if len(text) > 0 {
			return []byte{text[0]}
		}
		return []byte{0}
	case OIDBytea:
		return byteaFromText(text)
	case OIDUUID:
		if u, ok := encodeCopyUUID(text); ok {
			return u
		}
		return []byte(text)
	case OIDDate:
		if d, ok := encodeCopyDate(text); ok {
			return d
		}
		return []byte(text)
	case OIDTimestamp, OIDTimestamptz:
		if ts, ok := encodeCopyTimestamp(text); ok {
			return ts
		}
		return []byte(text)
	case OIDJSONB:
		// jsonb binary: 1 version byte (0x01) followed by the JSON text bytes.
		out := make([]byte, 0, len(text)+1)
		out = append(out, 0x01)
		out = append(out, text...)
		return out
	case OIDNumeric:
		if n, ok := encodeCopyNumeric(text); ok {
			return n
		}
		// NaN already handled inside encodeCopyNumeric; an unparseable value
		// falls back to text bytes (round-trips since decode mirrors it).
		return []byte(text)
	default:
		// text/varchar/bpchar/name/json/unknown: binary == UTF-8 bytes.
		return []byte(text)
	}
}

// decodeCopyBinaryField converts a binary wire payload to PG text for the OID.
func decodeCopyBinaryField(b []byte, oid uint32) (string, error) {
	switch oid {
	case OIDInt2:
		if len(b) != 2 {
			return "", newExecError("22P03", "COPY binary int2 wrong length %d", len(b))
		}
		return strconv.FormatInt(int64(int16(binary.BigEndian.Uint16(b))), 10), nil
	case OIDInt4:
		if len(b) != 4 {
			return "", newExecError("22P03", "COPY binary int4 wrong length %d", len(b))
		}
		return strconv.FormatInt(int64(int32(binary.BigEndian.Uint32(b))), 10), nil
	case OIDOid:
		if len(b) != 4 {
			return "", newExecError("22P03", "COPY binary oid wrong length %d", len(b))
		}
		return strconv.FormatUint(uint64(binary.BigEndian.Uint32(b)), 10), nil
	case OIDInt8:
		if len(b) != 8 {
			return "", newExecError("22P03", "COPY binary int8 wrong length %d", len(b))
		}
		return strconv.FormatInt(int64(binary.BigEndian.Uint64(b)), 10), nil
	case OIDFloat4:
		if len(b) != 4 {
			return "", newExecError("22P03", "COPY binary float4 wrong length %d", len(b))
		}
		f := math.Float32frombits(binary.BigEndian.Uint32(b))
		return strconv.FormatFloat(float64(f), 'g', -1, 32), nil
	case OIDFloat8:
		if len(b) != 8 {
			return "", newExecError("22P03", "COPY binary float8 wrong length %d", len(b))
		}
		f := math.Float64frombits(binary.BigEndian.Uint64(b))
		return strconv.FormatFloat(f, 'g', -1, 64), nil
	case OIDBool:
		if len(b) != 1 {
			return "", newExecError("22P03", "COPY binary bool wrong length %d", len(b))
		}
		if b[0] != 0 {
			return "t", nil
		}
		return "f", nil
	case OIDChar:
		return string(b), nil
	case OIDBytea:
		return byteaToText(b), nil
	case OIDUUID:
		if len(b) != 16 {
			return "", newExecError("22P03", "COPY binary uuid wrong length %d", len(b))
		}
		h := hex.EncodeToString(b)
		return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
	case OIDDate:
		if len(b) != 4 {
			return "", newExecError("22P03", "COPY binary date wrong length %d", len(b))
		}
		days := int32(binary.BigEndian.Uint32(b))
		t := copyBinaryEpoch.AddDate(0, 0, int(days))
		return t.Format("2006-01-02"), nil
	case OIDTimestamp, OIDTimestamptz:
		if len(b) != 8 {
			return "", newExecError("22P03", "COPY binary timestamp wrong length %d", len(b))
		}
		micros := int64(binary.BigEndian.Uint64(b))
		t := copyBinaryEpoch.Add(time.Duration(micros) * time.Microsecond)
		return t.Format("2006-01-02 15:04:05.999999"), nil
	case OIDJSONB:
		if len(b) < 1 {
			return "", newExecError("22P03", "COPY binary jsonb empty")
		}
		if b[0] != 0x01 {
			return "", newExecError("22P03", "COPY binary jsonb unsupported version %d", b[0])
		}
		return string(b[1:]), nil
	case OIDNumeric:
		return decodeCopyNumeric(b)
	default:
		return string(b), nil
	}
}

// --- per-type helpers ---

func encodeCopyUUID(text string) ([]byte, bool) {
	hexStr := strings.ReplaceAll(strings.TrimSpace(text), "-", "")
	if len(hexStr) != 32 {
		return nil, false
	}
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, false
	}
	return b, true
}

func encodeCopyTimestamp(text string) ([]byte, bool) {
	t, ok := copyParseTime(text)
	if !ok {
		return nil, false
	}
	micros := t.UTC().Sub(copyBinaryEpoch).Microseconds()
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(micros))
	return b[:], true
}

func encodeCopyDate(text string) ([]byte, bool) {
	t, ok := copyParseTime(text)
	if !ok {
		return nil, false
	}
	days := int32(math.Round(t.UTC().Sub(copyBinaryEpoch).Hours() / 24))
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(days))
	return b[:], true
}

// PG numeric binary format (numeric_send / numeric_recv):
//   int16 ndigits | int16 weight | uint16 sign | uint16 dscale | ndigits*int16
// digits are base-10000 (NBASE). sign: 0x0000 pos, 0x4000 neg, 0xC000 NaN.
const (
	numericPos uint16 = 0x0000
	numericNeg uint16 = 0x4000
	numericNaN uint16 = 0xC000
)

// encodeCopyNumeric encodes a decimal text value to PG numeric binary form.
func encodeCopyNumeric(text string) ([]byte, bool) {
	s := strings.TrimSpace(text)
	if strings.EqualFold(s, "nan") {
		var b []byte
		b = appendInt16(b, 0)               // ndigits
		b = appendInt16(b, 0)               // weight
		b = appendUint16(b, numericNaN)     // sign
		b = appendInt16(b, 0)               // dscale
		return b, true
	}
	sign := numericPos
	if strings.HasPrefix(s, "-") {
		sign = numericNeg
		s = s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	intPart, fracPart := s, ""
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		intPart, fracPart = s[:dot], s[dot+1:]
	}
	dscale := len(fracPart) // decimal digits after the point
	// Validate digits.
	for _, r := range intPart + fracPart {
		if r < '0' || r > '9' {
			return nil, false
		}
	}
	if intPart == "" {
		intPart = "0"
	}
	// Build the digit string and remember where the decimal point sits relative
	// to base-10000 groups. Pad the integer part on the left so its length is a
	// multiple of 4, and the fractional part on the right likewise.
	intPad := (4 - len(intPart)%4) % 4
	intDigits := strings.Repeat("0", intPad) + intPart
	fracPad := (4 - len(fracPart)%4) % 4
	fracDigits := fracPart + strings.Repeat("0", fracPad)

	// weight is the base-10000 exponent of the most significant NBASE digit:
	// number of integer NBASE groups minus 1.
	intGroups := len(intDigits) / 4
	weight := intGroups - 1

	all := intDigits + fracDigits
	var digits []int16
	for i := 0; i < len(all); i += 4 {
		g, err := strconv.Atoi(all[i : i+4])
		if err != nil {
			return nil, false
		}
		digits = append(digits, int16(g))
	}
	// Trim leading zero groups (adjust weight) and trailing zero groups.
	for len(digits) > 0 && digits[0] == 0 {
		digits = digits[1:]
		weight--
	}
	for len(digits) > 0 && digits[len(digits)-1] == 0 {
		digits = digits[:len(digits)-1]
	}
	if len(digits) == 0 {
		// Value is exactly zero.
		weight = 0
	}

	var b []byte
	b = appendInt16(b, int16(len(digits)))
	b = appendInt16(b, int16(weight))
	b = appendUint16(b, sign)
	b = appendInt16(b, int16(dscale))
	for _, d := range digits {
		b = appendInt16(b, d)
	}
	return b, true
}

// decodeCopyNumeric decodes PG numeric binary form back to canonical decimal text.
func decodeCopyNumeric(b []byte) (string, error) {
	if len(b) < 8 {
		return "", newExecError("22P03", "COPY binary numeric header too short")
	}
	ndigits := int(int16(binary.BigEndian.Uint16(b[0:])))
	weight := int(int16(binary.BigEndian.Uint16(b[2:])))
	sign := binary.BigEndian.Uint16(b[4:])
	dscale := int(int16(binary.BigEndian.Uint16(b[6:])))
	if sign == numericNaN {
		return "NaN", nil
	}
	if ndigits < 0 || 8+ndigits*2 > len(b) {
		return "", newExecError("22P03", "COPY binary numeric digit count out of range")
	}
	digits := make([]int, ndigits)
	for i := 0; i < ndigits; i++ {
		digits[i] = int(int16(binary.BigEndian.Uint16(b[8+i*2:])))
	}
	// Reconstruct the exact decimal value as big.Rat then format with dscale.
	// value = sum(digit[i] * 10000^(weight-i)).
	nbase := big.NewInt(10000)
	rat := new(big.Rat)
	for i, d := range digits {
		exp := weight - i
		term := new(big.Rat).SetInt64(int64(d))
		scale := new(big.Rat)
		if exp >= 0 {
			scale.SetInt(new(big.Int).Exp(nbase, big.NewInt(int64(exp)), nil))
		} else {
			den := new(big.Int).Exp(nbase, big.NewInt(int64(-exp)), nil)
			scale.SetFrac(big.NewInt(1), den)
		}
		term.Mul(term, scale)
		rat.Add(rat, term)
	}
	out := rat.FloatString(dscale)
	if sign == numericNeg && !isZeroDecimal(out) {
		out = "-" + out
	}
	return out, nil
}

func isZeroDecimal(s string) bool {
	for _, r := range s {
		if r != '0' && r != '.' {
			return false
		}
	}
	return true
}

// --- little binary append helpers ---

func appendInt16(b []byte, v int16) []byte {
	return append(b, byte(uint16(v)>>8), byte(uint16(v)))
}

func appendUint16(b []byte, v uint16) []byte {
	return append(b, byte(v>>8), byte(v))
}

func appendInt32(b []byte, v int32) []byte {
	u := uint32(v)
	return append(b, byte(u>>24), byte(u>>16), byte(u>>8), byte(u))
}
