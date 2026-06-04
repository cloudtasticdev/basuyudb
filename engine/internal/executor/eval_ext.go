package executor

import (
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
)

// powOp implements the `^` exponentiation operator. Following PostgreSQL,
// numeric ^ numeric → numeric (computed via big.Float), otherwise the operands
// are treated as float8 and math.Pow is used (→ float8).
func powOp(a, b value) (value, error) {
	if a.oid == OIDNumeric || b.oid == OIDNumeric {
		base, ok := new(big.Float).SetPrec(numericPrecision).SetString(a.text)
		if !ok {
			return value{}, numErr(a.text)
		}
		expf, err := strconv.ParseFloat(b.text, 64)
		if err != nil {
			return value{}, numErr(b.text)
		}
		// Integer exponent: exact repeated multiplication keeps full precision.
		if expf == math.Trunc(expf) && math.Abs(expf) < 1e6 {
			n := int64(expf)
			result := new(big.Float).SetPrec(numericPrecision).SetInt64(1)
			absN := n
			if absN < 0 {
				absN = -absN
			}
			for i := int64(0); i < absN; i++ {
				result.Mul(result, base)
			}
			if n < 0 {
				if result.Sign() == 0 {
					return value{}, newExecError("22012", "division by zero")
				}
				one := new(big.Float).SetPrec(numericPrecision).SetInt64(1)
				result.Quo(one, result)
			}
			return value{text: formatBigFloat(result), oid: OIDNumeric}, nil
		}
		// Fractional exponent: fall back to float64 math, returned as numeric.
		basef, _ := base.Float64()
		r := math.Pow(basef, expf)
		bf := new(big.Float).SetPrec(numericPrecision).SetFloat64(r)
		return value{text: formatBigFloat(bf), oid: OIDNumeric}, nil
	}
	af, err := strconv.ParseFloat(a.text, 64)
	if err != nil {
		return value{}, numErr(a.text)
	}
	bf, err := strconv.ParseFloat(b.text, 64)
	if err != nil {
		return value{}, numErr(b.text)
	}
	return value{text: strconv.FormatFloat(math.Pow(af, bf), 'g', -1, 64), oid: OIDFloat8}, nil
}

// arrayOverlap implements the array `&&` overlap operator: true if the two
// arrays share at least one element. Mirrors the @>/<@ containment path.
func arrayOverlap(a, b value) value {
	ea := pgArrayElements(a.text)
	eb := pgArrayElements(b.text)
	set := make(map[string]bool, len(ea))
	for _, e := range ea {
		set[e] = true
	}
	for _, e := range eb {
		if set[e] {
			return boolValue(true)
		}
	}
	return boolValue(false)
}

// rowValuesText is the sentinel oid-agnostic encoding for a ROW(...) value. A
// row is represented as a PG-style parenthesised list "(a,b,c)" so it round-
// trips through the text protocol and can be matched in IN-lists. We also keep
// the decomposed datums via rowElems for lexicographic comparison.

// evalRowExpr evaluates a ROW(...) constructor into a record-style value. The
// text form is "(elem1,elem2,...)"; the oid is OIDRecord so comparison and IN
// can detect row operands.
func (ev *evaluator) evalRowExpr(r *ast.RowExpr) (value, error) {
	elems, err := ev.evalRowItems(r)
	if err != nil {
		return value{}, err
	}
	return value{text: encodeRowText(elems), oid: OIDRecord}, nil
}

// evalFieldSelect evaluates composite-type field access `(expr).field`. It
// resolves the composite type of Arg (from a ROW(...)::typename cast or from a
// composite-typed column reference), evaluates Arg to a record value, decodes it,
// and returns the named field's value.
//
// Supported forms:
//   - (ROW(...)::typename).field  — type named by the cast
//   - (compositecol).field        — type from the column's declared composite type
//
// An unknown field, or an arg whose composite type cannot be determined, yields a
// clear error rather than a crash.
func (ev *evaluator) evalFieldSelect(fs *ast.FieldSelect) (value, error) {
	ct, err := ev.resolveCompositeType(fs.Arg)
	if err != nil {
		return value{}, err
	}
	if ct == nil {
		// No declared composite type for the operand. Try the broadened cases:
		// anonymous-record positional access `.fN` on a ROW(...)/record value
		// (including a nested `((x).a).b` where the intermediate is itself a
		// record, and records returned by scalar subqueries/functions).
		return ev.evalAnonRecordField(fs)
	}
	idx := ct.fieldIndex(fs.Field)
	if idx < 0 {
		return value{}, newExecError("42703",
			"column %q not found in composite type %q", fs.Field, ct.Name)
	}
	v, err := ev.eval(fs.Arg)
	if err != nil {
		return value{}, err
	}
	if v.null {
		return value{null: true}, nil
	}
	elems, ok := decodeRecordText(v.text)
	if !ok {
		return value{}, newExecError("22P02",
			"malformed record literal %q for composite type %q", v.text, ct.Name)
	}
	if idx >= len(elems) {
		// Fewer elements than fields (e.g. trailing NULLs omitted): treat as NULL.
		return value{null: true}, nil
	}
	out := elems[idx]
	if out.oid == 0 {
		out.oid = ct.Fields[idx].TypeOID
	}
	return out, nil
}

// anonFieldIndex parses an anonymous-record field name of the form "fN" (the
// names PostgreSQL assigns to fields of an untyped record/ROW value, 1-based),
// returning the 0-based position and ok. A non-"fN" field name yields ok=false.
func anonFieldIndex(field string) (int, bool) {
	if len(field) < 2 || (field[0] != 'f' && field[0] != 'F') {
		return 0, false
	}
	n, err := strconv.Atoi(field[1:])
	if err != nil || n < 1 {
		return 0, false
	}
	return n - 1, true
}

// evalAnonRecordField handles field access on a value whose composite type is
// not declared in the catalog. It supports:
//   - positional `.fN` access on an anonymous record (ROW(...), a record-typed
//     scalar subquery, or a function returning record);
//   - nested `((x).a).b` — fs.Arg is itself a FieldSelect; it is evaluated first
//     and, if it yields a record value, `.fN` positional access is applied.
//
// If the operand evaluates to a record value but the field is not a positional
// `fN` name (or N is out of range), a clear 42703 is returned. If the operand is
// not a record at all, a 42809 is returned. Neither path crashes.
func (ev *evaluator) evalAnonRecordField(fs *ast.FieldSelect) (value, error) {
	v, err := ev.eval(fs.Arg)
	if err != nil {
		return value{}, err
	}
	if v.null {
		return value{null: true}, nil
	}
	// A value supports positional/anon field access when it is a record OID or
	// at least record-shaped text "(...)" — the latter covers a nested
	// ((x).a).b where the intermediate field carries text "(...)" but defaulted
	// to OIDText rather than OIDRecord.
	elems, ok := decodeRecordText(v.text)
	if v.oid != OIDRecord && !ok {
		return value{}, newExecError("42809",
			"cannot extract field %q: operand is not a known composite type", fs.Field)
	}
	if !ok {
		return value{}, newExecError("22P02",
			"malformed record literal %q", v.text)
	}
	idx, ok := anonFieldIndex(fs.Field)
	if !ok {
		return value{}, newExecError("42703",
			"field %q not found in anonymous record (use positional f1, f2, ...)", fs.Field)
	}
	if idx >= len(elems) {
		return value{}, newExecError("42703",
			"record has %d fields, cannot access field %q", len(elems), fs.Field)
	}
	out := elems[idx]
	if out.oid == 0 {
		// If the extracted element is itself record-shaped text, mark it as a
		// record so a further `.fN` (nested access) recognizes it.
		if _, isRec := decodeRecordText(out.text); isRec {
			out.oid = OIDRecord
		} else {
			out.oid = OIDText
		}
	}
	return out, nil
}

// resolveCompositeType determines the composite type of a FieldSelect argument:
// a TypeCast names it directly; a column reference is resolved via fieldColType.
// Returns (nil, nil) when the type is not a known composite (caller errors).
func (ev *evaluator) resolveCompositeType(arg ast.Node) (*compositeType, error) {
	if ev.lookupComposite == nil {
		return nil, nil
	}
	switch a := arg.(type) {
	case *ast.TypeCast:
		if ct, ok := ev.lookupComposite(a.TypeName); ok {
			return ct, nil
		}
		return nil, nil
	case *ast.ColumnRef:
		if ev.fieldColType != nil {
			if tn, ok := ev.fieldColType(a.Fields); ok {
				if ct, ok := ev.lookupComposite(tn); ok {
					return ct, nil
				}
			}
		}
		return nil, nil
	}
	return nil, nil
}

// evalRowItems evaluates each item of a RowExpr into a slice of values.
func (ev *evaluator) evalRowItems(r *ast.RowExpr) ([]value, error) {
	out := make([]value, len(r.Items))
	for i, it := range r.Items {
		v, err := ev.eval(it)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// encodeRowText renders row element values as a PostgreSQL record literal
// "(a,b,c)", quoting elements that contain commas/quotes/parens.
func encodeRowText(elems []value) string {
	var b strings.Builder
	b.WriteByte('(')
	for i, e := range elems {
		if i > 0 {
			b.WriteByte(',')
		}
		if e.null {
			continue
		}
		s := e.text
		if strings.ContainsAny(s, `(),"`) || s == "" {
			b.WriteByte('"')
			b.WriteString(strings.ReplaceAll(s, `"`, `""`))
			b.WriteByte('"')
		} else {
			b.WriteString(s)
		}
	}
	b.WriteByte(')')
	return b.String()
}

// compareRows lexicographically compares two slices of row element values for
// the given comparison operator and returns a bool value (NULL if any
// element comparison is NULL and undecided). Mirrors PostgreSQL row comparison.
func compareRows(op string, a, b []value) (value, error) {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	cmp := 0
	sawNull := false
	for i := 0; i < n; i++ {
		if a[i].null || b[i].null {
			// Equality-style: a NULL element makes the result NULL unless a
			// prior element already decided <,>. For =, an undecided NULL → NULL.
			sawNull = true
			continue
		}
		c, err := compareCmp(a[i], b[i])
		if err != nil {
			return value{}, err
		}
		if c != 0 {
			cmp = c
			// First differing non-null element decides ordering operators.
			return rowCmpResult(op, cmp), nil
		}
	}
	if sawNull && (op == "=" || op == "<>" || op == "!=") {
		// All compared-equal so far but a NULL is unresolved → UNKNOWN.
		return value{null: true, oid: OIDBool}, nil
	}
	if len(a) != len(b) && cmp == 0 {
		if len(a) < len(b) {
			cmp = -1
		} else {
			cmp = 1
		}
	}
	return rowCmpResult(op, cmp), nil
}

// compareCmp returns -1/0/1 comparing two scalar values, reusing compare().
func compareCmp(a, b value) (int, error) {
	lt, err := compare("<", a, b)
	if err != nil {
		return 0, err
	}
	if asBool(lt) {
		return -1, nil
	}
	gt, err := compare(">", a, b)
	if err != nil {
		return 0, err
	}
	if asBool(gt) {
		return 1, nil
	}
	return 0, nil
}

func rowCmpResult(op string, cmp int) value {
	var res bool
	switch op {
	case "=":
		res = cmp == 0
	case "<>", "!=":
		res = cmp != 0
	case "<":
		res = cmp < 0
	case ">":
		res = cmp > 0
	case "<=":
		res = cmp <= 0
	case ">=":
		res = cmp >= 0
	}
	return boolValue(res)
}

// srfElements returns the expanded element values for a set-returning function
// call (unnest / jsonb_array_elements / jsonb_array_elements_text), and true if
// the call is a recognised SRF. The evaluator is used to evaluate the argument.
func (ev *evaluator) srfElements(f *ast.FuncCall) ([]value, bool, error) {
	name := strings.ToLower(strings.Join(f.FuncName, "."))
	switch name {
	case "unnest":
		var all []value
		for _, an := range f.Args {
			v, err := ev.eval(an)
			if err != nil {
				return nil, true, err
			}
			if v.null {
				continue
			}
			for _, e := range pgArrayElements(v.text) {
				all = append(all, value{text: e, oid: arrayElemOID(v.oid)})
			}
		}
		return all, true, nil
	case "jsonb_array_elements", "json_array_elements",
		"jsonb_array_elements_text", "json_array_elements_text":
		if len(f.Args) < 1 {
			return nil, true, newExecError("42883", "%s requires 1 argument", name)
		}
		v, err := ev.eval(f.Args[0])
		if err != nil {
			return nil, true, err
		}
		asText := strings.HasSuffix(name, "_text")
		elems, err := jsonbArrayElements(v, asText)
		if err != nil {
			return nil, true, err
		}
		return elems, true, nil
	}
	return nil, false, nil
}

// arrayElemOID maps an array OID to its element OID for unnest results.
func arrayElemOID(arrOID uint32) uint32 {
	switch arrOID {
	case OIDInt4Arr:
		return OIDInt4
	case OIDInt8Arr:
		return OIDInt8
	case OIDBoolArr:
		return OIDBool
	case OIDFloat8Arr:
		return OIDFloat8
	default:
		return OIDText
	}
}

// isSRFName reports whether name is a set-returning function handled by
// srfElements / materializeSRF.
func isSRFName(name string) bool {
	switch strings.ToLower(name) {
	case "unnest", "jsonb_array_elements", "json_array_elements",
		"jsonb_array_elements_text", "json_array_elements_text":
		return true
	}
	return false
}
