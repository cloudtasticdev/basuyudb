package executor

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/version"
)

// value is an intermediate evaluation result with its PG type OID.
type value struct {
	null bool
	text string // PG text-format representation
	oid uint32
}

// evaluator evaluates constant / parameter / arithmetic / comparison / boolean
// expressions. When resolveCol is non-nil the evaluator also resolves column
// references against the current row (table-scan context); when nil, a column
// reference is an error (constant context, e.g. Gate-1 SELECT).
type evaluator struct {
	params []Datum
	resolveCol func(fields []string) (value, error)
	// group, when non-nil, is the set of rows in the current GROUP BY bucket;
	// aggregate function calls (COUNT/SUM/AVG/MIN/MAX) reduce over it.
	group []boundRow
	// runSub, when non-nil, executes an (uncorrelated) scalar or IN subquery.
	runSub func(*ast.SelectStmt) (*Result, error)
	// windowVals holds precomputed per-row values for window function calls, and
	// rowIdx is the current output row; set during windowed projection.
	windowVals map[ast.Node][]value
	rowIdx int
	// sess, when non-nil, gives expression functions access to the session: GUC
	// reads/writes (current_setting / set_config) and the authenticated identity
	// (current_user / session_user). Threaded through so RLS policy predicates and
	// SET-driven GUCs evaluate correctly. nil in pure-constant contexts.
	sess *session.Session
	// lookupComposite, when non-nil, resolves a composite type by name (for
	// FieldSelect on a ROW(...)::typename value). nil in contexts without catalog
	// access; FieldSelect then maps a field only when the type is known another way.
	lookupComposite func(name string) (*compositeType, bool)
	// fieldColType, when non-nil, returns the declared composite type name of a
	// column reference (so (compositecol).field can decode by the column's type).
	fieldColType func(fields []string) (string, bool)
}

// asBool interprets a value as a SQL boolean for WHERE/HAVING/ON predicates.
// NULL is treated as false (three-valued logic collapses to "not true").
func asBool(v value) bool {
	if v.null {
		return false
	}
	switch v.oid {
	case OIDBool:
		return v.text == "t" || v.text == "true"
	default:
		// Non-bool in boolean position: treat non-empty/non-zero as true.
		return v.text != "" && v.text != "0" && v.text != "f" && v.text != "false"
	}
}

func (ev *evaluator) eval(n ast.Node) (value, error) {
	switch e := n.(type) {
	case *ast.A_Const:
		return evalConst(e), nil

	case *ast.ParamRef:
		idx := e.Number - 1
		if idx < 0 || idx >= len(ev.params) {
			return value{}, newExecError("42P02", "bind parameter $%d out of range", e.Number)
		}
		p := ev.params[idx]
		return value{null: p.Null, text: p.Text, oid: OIDText}, nil

	case *ast.A_Expr:
		return ev.evalAExpr(e)

	case *ast.RowExpr:
		return ev.evalRowExpr(e)

	case *ast.FieldSelect:
		return ev.evalFieldSelect(e)

	case *ast.SubLink:
		return ev.evalScalarSub(e)

	case *ast.FuncCall:
		if e.Over != nil {
			return ev.evalWindowRef(e)
		}
		return ev.evalFunc(e)

	case *ast.CaseExpr:
		return ev.evalCase(e)

	case *ast.TypeCast:
		// Milestone-1: evaluate the inner expression and report the cast type's
		// OID where known; value text is passed through.
		v, err := ev.eval(e.Arg)
		if err != nil {
			return value{}, err
		}
		v.oid = oidForTypeName(e.TypeName)
		return v, nil

	case *ast.BoolExpr:
		return ev.evalBool(e)

	case *ast.NullTest:
		v, err := ev.eval(e.Arg)
		if err != nil {
			return value{}, err
		}
		res := v.null == e.TestNull
		return boolValue(res), nil

	case *ast.ColumnRef:
		// Niladic SQL keyword-functions written without parentheses
		// (current_user, CURRENT_TIMESTAMP, current_date, ...).
		if len(e.Fields) == 1 {
			// User-identity keywords resolve to the session's authenticated user so
			// RLS policies comparing a column to current_user behave correctly.
			if ev.sess != nil {
				switch strings.ToLower(e.Fields[0]) {
				case "current_user", "session_user", "user", "current_role":
					return value{text: ev.sess.User(), oid: OIDText}, nil
				}
			}
			if v, ok := niladicKeyword(e.Fields[0]); ok {
				return v, nil
			}
		}
		if ev.resolveCol == nil {
			return value{}, newExecError("42703", "column %q cannot be resolved without a FROM clause", strings.Join(e.Fields, "."))
		}
		return ev.resolveCol(e.Fields)

	default:
		return value{}, newExecError("0A000", "unsupported expression %T", n)
	}
}

// evalBool handles AND / OR / NOT with SQL-ish three-valued logic collapsed so
// that NULL behaves as "not true" in predicate position.
func (ev *evaluator) evalBool(b *ast.BoolExpr) (value, error) {
	switch b.Op {
	case ast.NOT_EXPR:
		v, err := ev.eval(b.Args[0])
		if err != nil {
			return value{}, err
		}
		return boolValue(!asBool(v)), nil
	case ast.AND_EXPR:
		for _, a := range b.Args {
			v, err := ev.eval(a)
			if err != nil {
				return value{}, err
			}
			if !asBool(v) {
				return boolValue(false), nil
			}
		}
		return boolValue(true), nil
	case ast.OR_EXPR:
		for _, a := range b.Args {
			v, err := ev.eval(a)
			if err != nil {
				return value{}, err
			}
			if asBool(v) {
				return boolValue(true), nil
			}
		}
		return boolValue(false), nil
	}
	return value{}, newExecError("XX000", "unknown boolean operator")
}

func boolValue(b bool) value {
	t := "f"
	if b {
		t = "t"
	}
	return value{text: t, oid: OIDBool}
}

func evalConst(c *ast.A_Const) value {
	switch c.Type {
	case ast.ConstNull:
		return value{null: true, oid: OIDUnknown}
	case ast.ConstInt:
		return value{text: c.Val, oid: OIDInt4}
	case ast.ConstFloat:
		return value{text: c.Val, oid: OIDFloat8}
	case ast.ConstBool:
		t := "f"
		if c.Val == "true" {
			t = "t"
		}
		return value{text: t, oid: OIDBool}
	default: // ConstString
		return value{text: c.Val, oid: OIDText}
	}
}

// evalAExpr handles unary minus and binary arithmetic/comparison over numeric
// and text constants. Comparison yields a bool; arithmetic yields int4/float8.
func (ev *evaluator) evalAExpr(e *ast.A_Expr) (value, error) {
	if e.Kind == ast.AEXPR_IN {
		return ev.evalIn(e)
	}
	if e.Kind == ast.AEXPR_LIKE || e.Kind == ast.AEXPR_ILIKE {
		lv, err := ev.eval(e.Lexpr)
		if err != nil {
			return value{}, err
		}
		rv, err := ev.eval(e.Rexpr)
		if err != nil {
			return value{}, err
		}
		if lv.null || rv.null {
			return value{null: true, oid: OIDBool}, nil
		}
		if e.Kind == ast.AEXPR_LIKE {
			return boolValue(pgLike(lv.text, rv.text)), nil
		}
		return boolValue(pgILike(lv.text, rv.text)), nil
	}
	if e.Kind == ast.AEXPR_SIMILAR_TO || e.Kind == ast.AEXPR_NOT_SIMILAR_TO {
		lv, err := ev.eval(e.Lexpr)
		if err != nil {
			return value{}, err
		}
		rv, err := ev.eval(e.Rexpr)
		if err != nil {
			return value{}, err
		}
		if lv.null || rv.null {
			return value{null: true, oid: OIDBool}, nil
		}
		matched, _ := regexp.MatchString(similarToRegex(rv.text), lv.text)
		if e.Kind == ast.AEXPR_NOT_SIMILAR_TO {
			matched = !matched
		}
		return boolValue(matched), nil
	}
	// Unary (Lexpr nil): currently only "-" / "+".
	if e.Lexpr == nil {
		r, err := ev.eval(e.Rexpr)
		if err != nil {
			return value{}, err
		}
		if e.Name == "-" {
			return negate(r)
		}
		return r, nil
	}

	// Handle col = ANY(array) / col = ALL(array) before generic evaluation so that
	// the "any"/"all" pseudo-function is intercepted here and never reaches
	// evalScalarFunc (which would error "function does not exist").
	if e.Name == "=" || e.Name == "<>" || e.Name == "!=" {
		if fc, ok := e.Rexpr.(*ast.FuncCall); ok {
			fname := strings.ToLower(strings.Join(fc.FuncName, "."))
			if (fname == "any" || fname == "all") && len(fc.Args) == 1 {
				return ev.evalQuantifiedComparison(e.Name, fname, e.Lexpr, fc.Args[0])
			}
		}
	}

	// Row comparison: (a,b) op (c,d) compares element-wise / lexicographically.
	if lrow, lok := e.Lexpr.(*ast.RowExpr); lok {
		if rrow, rok := e.Rexpr.(*ast.RowExpr); rok {
			switch e.Name {
			case "=", "<>", "!=", "<", ">", "<=", ">=":
				la, err := ev.evalRowItems(lrow)
				if err != nil {
					return value{}, err
				}
				ra, err := ev.evalRowItems(rrow)
				if err != nil {
					return value{}, err
				}
				return compareRows(e.Name, la, ra)
			}
		}
	}

	lv, err := ev.eval(e.Lexpr)
	if err != nil {
		return value{}, err
	}
	rv, err := ev.eval(e.Rexpr)
	if err != nil {
		return value{}, err
	}
	if lv.null || rv.null {
		return value{null: true, oid: OIDBool}, nil
	}

	switch e.Name {
	case "-":
		// JSONB delete operator: jsonb - text / int / text[]. Only when the
		// left operand is json/jsonb; otherwise fall through to numeric.
		if lv.oid == OIDJSONB || lv.oid == OIDJSON {
			rIsInt := rv.oid == OIDInt4 || rv.oid == OIDInt8 || rv.oid == OIDInt2
			return jsonbDelete(lv, rv, rIsInt)
		}
		return arith(e.Name, lv, rv)
	case "+", "*", "/", "%":
		return arith(e.Name, lv, rv)
	case "^":
		return powOp(lv, rv)
	case "&&":
		// Array overlap: true if the two arrays share any element.
		return arrayOverlap(lv, rv), nil
	case "=", "<>", "!=", "<", ">", "<=", ">=":
		return compare(e.Name, lv, rv)
	case "->>", "->", "#>>", "#>":
		return jsonbExtract(e.Name, lv, rv)
	case "@>", "<@":
		// Array / JSON containment.
		var container, contained value
		if e.Name == "@>" {
			container, contained = lv, rv
		} else {
			container, contained = rv, lv
		}
		// Array containment: {a,b,c} @> {a,b} — every element of contained is in container.
		if strings.HasPrefix(strings.TrimSpace(container.text), "{") {
			containerElems := pgArrayElements(container.text)
			containedElems := pgArrayElements(contained.text)
			containerSet := make(map[string]bool, len(containerElems))
			for _, elem := range containerElems {
				containerSet[elem] = true
			}
			for _, elem := range containedElems {
				if !containerSet[elem] {
					return boolValue(false), nil
				}
			}
			return boolValue(true), nil
		}
		// JSON object / array containment: simple substring heuristic.
		return boolValue(strings.Contains(container.text, contained.text)), nil
	case "?":
		// JSON key exists
		return boolValue(strings.Contains(lv.text, `"`+rv.text+`"`)), nil
	case "?|":
		// JSON key exists (any of array)
		for _, k := range pgArrayElements(rv.text) {
			if strings.Contains(lv.text, `"`+k+`"`) {
				return boolValue(true), nil
			}
		}
		return boolValue(false), nil
	case "?&":
		// JSON key exists (all of array)
		for _, k := range pgArrayElements(rv.text) {
			if !strings.Contains(lv.text, `"`+k+`"`) {
				return boolValue(false), nil
			}
		}
		return boolValue(true), nil
	case "||":
		// Text/array/JSON concatenation
		if lv.oid == OIDTextArr || rv.oid == OIDTextArr {
			e1 := pgArrayElements(lv.text); e2 := pgArrayElements(rv.text)
			return value{text: "{" + strings.Join(append(e1, e2...), ",") + "}", oid: OIDTextArr}, nil
		}
		return value{text: lv.text + rv.text, oid: OIDText}, nil
	case "~~":
		return boolValue(pgLike(lv.text, rv.text)), nil
	case "~~*":
		return boolValue(pgILike(lv.text, rv.text)), nil
	case "!~~":
		return boolValue(!pgLike(lv.text, rv.text)), nil
	case "!~~*":
		return boolValue(!pgILike(lv.text, rv.text)), nil
	case "~":
		matched, _ := regexp.MatchString(rv.text, lv.text)
		return boolValue(matched), nil
	case "~*":
		matched, _ := regexp.MatchString("(?i)"+rv.text, lv.text)
		return boolValue(matched), nil
	case "!~":
		matched, _ := regexp.MatchString(rv.text, lv.text)
		return boolValue(!matched), nil
	case "!~*":
		matched, _ := regexp.MatchString("(?i)"+rv.text, lv.text)
		return boolValue(!matched), nil
	default:
		return value{}, newExecError("0A000", "operator %q not supported", e.Name)
	}
}

// evalQuantifiedComparison evaluates col op ANY(array) or col op ALL(array).
// The array is a PG text-array literal like {public,information_schema} or a
// comma-separated value. Returns a bool value.
func (ev *evaluator) evalQuantifiedComparison(op, quantifier string, leftNode, arrNode ast.Node) (value, error) {
	lv, err := ev.eval(leftNode)
	if err != nil {
		return value{}, err
	}
	arrVal, err := ev.eval(arrNode)
	if err != nil {
		return value{}, err
	}
	if lv.null || arrVal.null {
		return value{null: true, oid: OIDBool}, nil
	}
	// Parse the PostgreSQL array literal {elem1,elem2,...}.
	elems := pgArrayElements(arrVal.text)

	for _, elem := range elems {
		cmp, err := compare(op, lv, value{text: elem, oid: lv.oid})
		if err != nil {
			continue
		}
		if quantifier == "any" && asBool(cmp) {
			return value{text: "t", oid: OIDBool}, nil
		}
		if quantifier == "all" && !asBool(cmp) {
			return value{text: "f", oid: OIDBool}, nil
		}
	}
	if quantifier == "any" {
		return value{text: "f", oid: OIDBool}, nil
	}
	// ALL: if all comparisons matched (or empty array), return true.
	return value{text: "t", oid: OIDBool}, nil
}

// pgArrayElements parses a PostgreSQL array literal {a,b,c} or a comma-
// separated value (fallback for non-standard representations).
func pgArrayElements(s string) []string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '{' && s[len(s)-1] == '}' {
		s = s[1 : len(s)-1]
	}
	if s == "" {
		return nil
	}
	// Split on commas respecting double-quoted elements.
	var elems []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuote = !inQuote
		case c == ',' && !inQuote:
			elems = append(elems, strings.Trim(cur.String(), `"`))
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	elems = append(elems, strings.Trim(cur.String(), `"`))
	return elems
}

func (ev *evaluator) evalFunc(f *ast.FuncCall) (value, error) {
	name := strings.ToLower(strings.Join(f.FuncName, "."))
	if isAggregateName(name) {
		if ev.group == nil {
			return value{}, newExecError("42803", "aggregate function %q is not allowed here", name)
		}
		return evalAggregate(name, f, ev.group, ev.params)
	}
	switch name {
	case "version":
		return value{text: "BasuyuDB " + version.Number + " on PostgreSQL 15 wire protocol", oid: OIDText}, nil
	case "current_database":
		return value{text: "defaultdb", oid: OIDText}, nil
	case "current_schema":
		return value{text: "public", oid: OIDText}, nil
	case "array_constructor":
		// ARRAY[expr, expr, ...] constructor — evaluates each element and
		// produces a PostgreSQL array literal {elem1,elem2,...}.
		if len(f.Args) == 0 {
			return value{text: "{}", oid: OIDTextArr}, nil
		}
		elems := make([]string, 0, len(f.Args))
		var elemOID uint32 = OIDText
		for _, a := range f.Args {
			v, err := ev.eval(a)
			if err != nil {
				return value{}, err
			}
			if v.null {
				elems = append(elems, "NULL")
			} else {
				elems = append(elems, v.text)
				elemOID = v.oid
			}
		}
		// Map element OID to array OID.
		arrOID := OIDTextArr
		switch elemOID {
		case OIDInt4: arrOID = OIDInt4Arr
		case OIDInt8: arrOID = OIDInt8Arr
		case OIDBool: arrOID = OIDBoolArr
		case OIDFloat8: arrOID = OIDFloat8Arr
		}
		return value{text: "{" + strings.Join(elems, ",") + "}", oid: arrOID}, nil

	case "array_from_subquery":
		// ARRAY(SELECT ...) — run the subquery and collect first column.
		if ev.runSub != nil && len(f.Args) == 1 {
			if sl, ok := f.Args[0].(*ast.SubLink); ok {
				if sel, ok2 := sl.SubSelect.(*ast.SelectStmt); ok2 {
					res, err := ev.runSub(sel)
					if err != nil {
						return value{}, err
					}
					elems := make([]string, 0, len(res.Rows))
					for _, row := range res.Rows {
						if len(row) > 0 && !row[0].Null {
							elems = append(elems, row[0].Text)
						}
					}
					return value{text: "{" + strings.Join(elems, ",") + "}", oid: OIDTextArr}, nil
				}
			}
		}
		return value{text: "{}", oid: OIDTextArr}, nil

	case "timezone":
		// timezone('UTC', timestamp) — AT TIME ZONE rewritten form
		if len(f.Args) < 2 {
			return value{null: true, oid: OIDTimestamptz}, nil
		}
		tzArg, err := ev.eval(f.Args[0])
		if err != nil { return value{}, err }
		tsArg, err := ev.eval(f.Args[1])
		if err != nil { return value{}, err }
		if tsArg.null { return value{null: true, oid: OIDTimestamptz}, nil }
		t, err2 := parseTimestamp(tsArg.text)
		if err2 != nil { return value{text: tsArg.text, oid: OIDTimestamptz}, nil }
		tz := strings.TrimSpace(tzArg.text)
		loc, err3 := time.LoadLocation(tz)
		if err3 != nil { loc = time.UTC }
		return value{text: t.In(loc).Format("2006-01-02 15:04:05"), oid: OIDTimestamptz}, nil
	}
	// General scalar function library (COALESCE/NULLIF/string/math/time).
	return ev.evalScalarFunc(name, f)
}

// evalScalarFunc implements the common scalar functions ORMs and query builders
// emit. Args are evaluated lazily where it matters (COALESCE short-circuits).
func (ev *evaluator) evalScalarFunc(name string, f *ast.FuncCall) (value, error) {
	switch name {
	case "now", "current_timestamp", "transaction_timestamp", "statement_timestamp", "clock_timestamp":
		return value{text: nowText(), oid: OIDTimestamptz}, nil
	case "current_date":
		return value{text: nowText()[:10], oid: OIDDate}, nil
	case "current_time", "localtime":
		return value{text: nowTimeText(), oid: OIDTime}, nil
	case "gen_random_uuid", "uuid_generate_v4":
		u, err := randomUUIDv4()
		if err != nil {
			return value{}, err
		}
		return value{text: u, oid: OIDUUID}, nil
	case "current_setting":
		// current_setting(name [, missing_ok]) → the run-time configuration value.
		// Reads a session-set GUC first (SET / set_config), then the built-in GUC
		// defaults. An unknown setting errors (SQLSTATE 42704) unless missing_ok is
		// TRUE, in which case it returns NULL — matching PostgreSQL.
		if len(f.Args) < 1 {
			return value{}, newExecError("42883", "current_setting requires at least 1 argument")
		}
		nv, err := ev.eval(f.Args[0])
		if err != nil {
			return value{}, err
		}
		missingOK := false
		if len(f.Args) >= 2 {
			mv, err := ev.eval(f.Args[1])
			if err != nil {
				return value{}, err
			}
			missingOK = asBool(mv)
		}
		if ev.sess != nil {
			if v, ok := ev.sess.GetSetting(nv.text); ok {
				return value{text: v, oid: OIDText}, nil
			}
		}
		if known, ok := gucValueKnown(nv.text); ok {
			return value{text: known, oid: OIDText}, nil
		}
		if missingOK {
			return value{null: true, oid: OIDText}, nil
		}
		return value{}, newExecError("42704", "unrecognized configuration parameter %q", nv.text)
	case "set_config":
		// set_config(name, value, is_local) → set the GUC and return the value.
		if len(f.Args) >= 2 {
			nv, err := ev.eval(f.Args[0])
			if err != nil {
				return value{}, err
			}
			v, err := ev.eval(f.Args[1])
			if err != nil {
				return value{}, err
			}
			if ev.sess != nil && !nv.null {
				ev.sess.SetSetting(nv.text, v.text)
			}
			return value{null: v.null, text: v.text, oid: OIDText}, nil
		}
		return value{null: true, oid: OIDText}, nil
	case "format_type":
		// format_type(typid, typmod) → text representation of a type OID.
		// Used by Prisma introspection. We return the type name based on the OID.
		if len(f.Args) >= 1 {
			v, err := ev.eval(f.Args[0])
			if err == nil && !v.null {
				if oid, e2 := strconv.ParseUint(v.text, 10, 32); e2 == nil {
					return value{text: sqlTypeName(uint32(oid)), oid: OIDText}, nil
				}
			}
		}
		return value{null: true, oid: OIDText}, nil
	case "obj_description", "col_description", "shobj_description":
		// Returns NULL — BasuyuDB doesn't store object comments.
		return value{null: true, oid: OIDText}, nil
	case "pg_get_constraintdef", "pg_get_expr", "pg_get_indexdef":
		// Returns NULL — stub for catalog queries that request DDL text.
		return value{null: true, oid: OIDText}, nil
	case "pg_backend_pid":
		return value{text: "1", oid: OIDInt4}, nil
	case "txid_current", "pg_current_xact_id":
		return value{text: "1", oid: OIDInt8}, nil
	case "pg_is_in_recovery":
		return value{text: "f", oid: OIDBool}, nil
	case "current_user", "session_user", "user", "current_role":
		if ev.sess != nil {
			return value{text: ev.sess.User(), oid: OIDText}, nil
		}
		return value{text: "postgres", oid: OIDText}, nil
	case "pg_postmaster_start_time", "pg_conf_load_time":
		return value{text: nowText(), oid: OIDTimestamptz}, nil
	case "coalesce":
		for _, a := range f.Args {
			v, err := ev.eval(a)
			if err != nil {
				return value{}, err
			}
			if !v.null {
				return v, nil
			}
		}
		return value{null: true, oid: OIDUnknown}, nil
	case "nullif":
		if len(f.Args) != 2 {
			return value{}, newExecError("42883", "nullif requires 2 arguments")
		}
		a, err := ev.eval(f.Args[0])
		if err != nil {
			return value{}, err
		}
		b, err := ev.eval(f.Args[1])
		if err != nil {
			return value{}, err
		}
		if !a.null && !b.null {
			if eq, err := compare("=", a, b); err == nil && asBool(eq) {
				return value{null: true, oid: a.oid}, nil
			}
		}
		return a, nil
	}

	// One-argument string/math helpers share an evaluate-first path.
	args, err := ev.evalArgs(f.Args)
	if err != nil {
		return value{}, err
	}
	switch name {
	case "length", "char_length", "character_length":
		if len(args) != 1 {
			return value{}, newExecError("42883", "%s requires 1 argument", name)
		}
		if args[0].null {
			return value{null: true, oid: OIDInt4}, nil
		}
		return value{text: strconv.Itoa(len([]rune(args[0].text))), oid: OIDInt4}, nil
	case "upper":
		return strFn(args, strings.ToUpper)
	case "lower":
		return strFn(args, strings.ToLower)
	case "trim", "btrim":
		return strFn(args, strings.TrimSpace)
	case "ltrim":
		return strFn(args, func(s string) string { return strings.TrimLeft(s, " ") })
	case "rtrim":
		return strFn(args, func(s string) string { return strings.TrimRight(s, " ") })
	case "concat":
		var b strings.Builder
		for _, a := range args {
			if !a.null {
				b.WriteString(a.text)
			}
		}
		return value{text: b.String(), oid: OIDText}, nil
	case "abs":
		if len(args) == 1 && !args[0].null && args[0].oid == OIDNumeric {
			f := parseBigFloat(args[0].text)
			f.Abs(f)
			return value{text: formatBigFloat(f), oid: OIDNumeric}, nil
		}
		return mathFn1(args, math.Abs)
	case "ceil", "ceiling":
		if len(args) == 1 && !args[0].null && args[0].oid == OIDNumeric {
			f := parseBigFloat(args[0].text)
			// big.Float has no Ceil; use Int + adjust if fractional part > 0
			i, acc := f.Int(nil)
			result := new(big.Float).SetPrec(numericPrecision).SetInt(i)
			if acc == big.Below { result.Add(result, new(big.Float).SetInt64(1)) }
			return value{text: formatBigFloat(result), oid: OIDNumeric}, nil
		}
		return mathFn1(args, math.Ceil)
	case "floor":
		if len(args) == 1 && !args[0].null && args[0].oid == OIDNumeric {
			f := parseBigFloat(args[0].text)
			i, acc := f.Int(nil)
			result := new(big.Float).SetPrec(numericPrecision).SetInt(i)
			if acc == big.Above { result.Sub(result, new(big.Float).SetInt64(1)) }
			return value{text: formatBigFloat(result), oid: OIDNumeric}, nil
		}
		return mathFn1(args, math.Floor)
	case "round":
		if len(args) == 1 && !args[0].null && args[0].oid == OIDNumeric {
			// round to nearest integer (banker's rounding via big.Float mode)
			f := parseBigFloat(args[0].text)
			// Add 0.5, then truncate (simple round-half-up)
			half := new(big.Float).SetPrec(numericPrecision).SetFloat64(0.5)
			if f.Sign() < 0 { half.Neg(half) }
			f.Add(f, half)
			i, _ := f.Int(nil)
			return value{text: i.String(), oid: OIDNumeric}, nil
		}
		if len(args) == 2 && !args[0].null && args[0].oid == OIDNumeric {
			// round(value, scale) — round to N decimal places
			places, _ := strconv.Atoi(args[1].text)
			f := parseBigFloat(args[0].text)
			scale := new(big.Float).SetPrec(numericPrecision)
			scale.SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(places)), nil))
			f.Mul(f, scale)
			half := new(big.Float).SetPrec(numericPrecision).SetFloat64(0.5)
			if f.Sign() < 0 { half.Neg(half) }
			f.Add(f, half)
			i, _ := f.Int(nil)
			result := new(big.Float).SetPrec(numericPrecision).SetInt(i)
			result.Quo(result, scale)
			return value{text: formatBigFloat(result), oid: OIDNumeric}, nil
		}
		return mathFn1(args, math.Round)
	case "sqrt":
		if len(args) == 1 && !args[0].null && args[0].oid == OIDNumeric {
			f := parseBigFloat(args[0].text)
			if f.Sign() < 0 { return value{}, newExecError("22023", "cannot take sqrt of negative number") }
			// Approximate sqrt via Newton-Raphson using big.Float
			result := new(big.Float).SetPrec(numericPrecision)
			// Use math.Sqrt as initial estimate, then one Newton step for precision
			flt, _ := f.Float64()
			result.SetFloat64(math.Sqrt(flt))
			// Newton step: r = (r + f/r) / 2
			tmp := new(big.Float).SetPrec(numericPrecision).Quo(f, result)
			result.Add(result, tmp)
			result.Quo(result, new(big.Float).SetInt64(2))
			return value{text: formatBigFloat(result), oid: OIDNumeric}, nil
		}
		return mathFn1(args, math.Sqrt)

	case "mod":
		// mod(y, x) → remainder of y/x; equivalent to y % x. Integer or numeric.
		if len(args) != 2 {
			return value{}, newExecError("42883", "mod requires 2 arguments")
		}
		if args[0].null || args[1].null {
			return value{null: true, oid: args[0].oid}, nil
		}
		return arith("%", args[0], args[1])

	// ── Date/time functions ────────────────────────────────────────────
	case "date_trunc":
		if len(args) < 2 {
			return value{}, newExecError("42883", "date_trunc requires 2 arguments")
		}
		if args[0].null || args[1].null {
			return value{null: true, oid: OIDTimestamptz}, nil
		}
		return dateTrunc(args[0].text, args[1].text)

	case "date_part", "extract":
		if len(args) < 2 {
			return value{}, newExecError("42883", "%s requires 2 arguments", name)
		}
		if args[0].null || args[1].null {
			return value{null: true, oid: OIDFloat8}, nil
		}
		return datePart(args[0].text, args[1].text)

	case "age":
		if len(args) == 1 {
			if args[0].null {
				return value{null: true, oid: OIDText}, nil
			}
			t, err := parseTimestamp(args[0].text)
			if err != nil {
				return value{null: true, oid: OIDText}, nil
			}
			d := time.Since(t)
			return value{text: formatInterval(d), oid: OIDText}, nil
		}
		if len(args) >= 2 {
			if args[0].null || args[1].null {
				return value{null: true, oid: OIDText}, nil
			}
			t1, e1 := parseTimestamp(args[0].text)
			t2, e2 := parseTimestamp(args[1].text)
			if e1 != nil || e2 != nil {
				return value{null: true, oid: OIDText}, nil
			}
			return value{text: formatInterval(t1.Sub(t2)), oid: OIDText}, nil
		}
		return value{null: true, oid: OIDText}, nil

	case "to_char":
		if len(args) < 2 {
			return value{null: true, oid: OIDText}, nil
		}
		if args[0].null {
			return value{null: true, oid: OIDText}, nil
		}
		return toCharFn(args[0], args[1].text)

	case "to_timestamp":
		if len(args) < 1 {
			return value{null: true, oid: OIDTimestamptz}, nil
		}
		if args[0].null {
			return value{null: true, oid: OIDTimestamptz}, nil
		}
		if len(args) == 1 {
			epoch, err := strconv.ParseFloat(args[0].text, 64)
			if err != nil {
				return value{null: true, oid: OIDTimestamptz}, nil
			}
			t := time.Unix(int64(epoch), 0).UTC()
			return value{text: t.Format("2006-01-02 15:04:05"), oid: OIDTimestamptz}, nil
		}
		return value{text: args[0].text, oid: OIDTimestamptz}, nil

	case "to_date":
		if len(args) < 1 || args[0].null {
			return value{null: true, oid: OIDDate}, nil
		}
		s := args[0].text
		if len(s) >= 10 {
			s = s[:10]
		}
		return value{text: s, oid: OIDDate}, nil

	case "make_date":
		if len(args) < 3 {
			return value{null: true, oid: OIDDate}, nil
		}
		y, _ := strconv.Atoi(args[0].text)
		mo, _ := strconv.Atoi(args[1].text)
		d, _ := strconv.Atoi(args[2].text)
		return value{text: time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC).Format("2006-01-02"), oid: OIDDate}, nil

	case "timeofday":
		return value{text: nowText(), oid: OIDText}, nil

	// ── String functions ───────────────────────────────────────────────
	case "substring", "substr":
		if len(args) < 2 {
			return value{null: true, oid: OIDText}, nil
		}
		if args[0].null {
			return value{null: true, oid: OIDText}, nil
		}
		r := []rune(args[0].text)
		start := 0
		if !args[1].null {
			n, _ := strconv.Atoi(args[1].text)
			start = n - 1
			if start < 0 {
				start = 0
			}
		}
		if start >= len(r) {
			return value{text: "", oid: OIDText}, nil
		}
		if len(args) >= 3 && !args[2].null {
			ln, _ := strconv.Atoi(args[2].text)
			end := start + ln
			if end > len(r) {
				end = len(r)
			}
			return value{text: string(r[start:end]), oid: OIDText}, nil
		}
		return value{text: string(r[start:]), oid: OIDText}, nil

	case "split_part":
		if len(args) < 3 || args[0].null {
			return value{null: true, oid: OIDText}, nil
		}
		parts := strings.Split(args[0].text, args[1].text)
		n, _ := strconv.Atoi(args[2].text)
		if n < 1 || n > len(parts) {
			return value{text: "", oid: OIDText}, nil
		}
		return value{text: parts[n-1], oid: OIDText}, nil

	case "string_to_array":
		if len(args) < 2 || args[0].null {
			return value{null: true, oid: OIDTextArr}, nil
		}
		parts := strings.Split(args[0].text, args[1].text)
		return value{text: "{" + strings.Join(parts, ",") + "}", oid: OIDTextArr}, nil

	case "array_to_string":
		if len(args) < 2 || args[0].null {
			return value{null: true, oid: OIDText}, nil
		}
		return value{text: strings.Join(pgArrayElements(args[0].text), args[1].text), oid: OIDText}, nil

	case "regexp_replace":
		if len(args) < 3 || args[0].null {
			return value{null: true, oid: OIDText}, nil
		}
		pattern := args[1].text
		if len(args) >= 4 && strings.Contains(args[3].text, "i") {
			pattern = "(?i)" + pattern
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return value{text: args[0].text, oid: OIDText}, nil
		}
		if len(args) >= 4 && strings.Contains(args[3].text, "g") {
			return value{text: re.ReplaceAllString(args[0].text, args[2].text), oid: OIDText}, nil
		}
		return value{text: re.ReplaceAllLiteralString(args[0].text, args[2].text), oid: OIDText}, nil

	case "regexp_match", "regexp_matches":
		if len(args) < 2 || args[0].null {
			return value{null: true, oid: OIDTextArr}, nil
		}
		re, err := regexp.Compile(args[1].text)
		if err != nil {
			return value{null: true, oid: OIDTextArr}, nil
		}
		m := re.FindStringSubmatch(args[0].text)
		if len(m) == 0 {
			return value{null: true, oid: OIDTextArr}, nil
		}
		if len(m) == 1 {
			return value{text: "{" + m[0] + "}", oid: OIDTextArr}, nil
		}
		return value{text: "{" + strings.Join(m[1:], ",") + "}", oid: OIDTextArr}, nil

	case "position", "strpos":
		if len(args) < 2 || args[0].null || args[1].null {
			return value{text: "0", oid: OIDInt4}, nil
		}
		idx := strings.Index(args[0].text, args[1].text)
		if idx < 0 {
			return value{text: "0", oid: OIDInt4}, nil
		}
		return value{text: strconv.Itoa(len([]rune(args[0].text[:idx])) + 1), oid: OIDInt4}, nil

	case "lpad":
		if len(args) < 2 || args[0].null {
			return value{null: true, oid: OIDText}, nil
		}
		n, _ := strconv.Atoi(args[1].text)
		fill := " "
		if len(args) >= 3 && !args[2].null && args[2].text != "" {
			fill = args[2].text
		}
		s := args[0].text
		for len([]rune(s)) < n {
			s = fill + s
		}
		rr := []rune(s)
		if len(rr) > n {
			s = string(rr[len(rr)-n:])
		}
		return value{text: s, oid: OIDText}, nil

	case "rpad":
		if len(args) < 2 || args[0].null {
			return value{null: true, oid: OIDText}, nil
		}
		n, _ := strconv.Atoi(args[1].text)
		fill := " "
		if len(args) >= 3 && !args[2].null && args[2].text != "" {
			fill = args[2].text
		}
		s := args[0].text
		for len([]rune(s)) < n {
			s = s + fill
		}
		rr := []rune(s)
		if len(rr) > n {
			s = string(rr[:n])
		}
		return value{text: s, oid: OIDText}, nil

	case "repeat":
		if len(args) < 2 || args[0].null {
			return value{null: true, oid: OIDText}, nil
		}
		n, _ := strconv.Atoi(args[1].text)
		if n < 0 {
			n = 0
		}
		return value{text: strings.Repeat(args[0].text, n), oid: OIDText}, nil

	case "replace":
		if len(args) < 3 || args[0].null {
			return value{null: true, oid: OIDText}, nil
		}
		return value{text: strings.ReplaceAll(args[0].text, args[1].text, args[2].text), oid: OIDText}, nil

	case "reverse":
		if len(args) < 1 || args[0].null {
			return value{null: true, oid: OIDText}, nil
		}
		rr := []rune(args[0].text)
		for i, j := 0, len(rr)-1; i < j; i, j = i+1, j-1 {
			rr[i], rr[j] = rr[j], rr[i]
		}
		return value{text: string(rr), oid: OIDText}, nil

	case "translate":
		if len(args) < 3 || args[0].null {
			return value{null: true, oid: OIDText}, nil
		}
		src, dst := []rune(args[1].text), []rune(args[2].text)
		m := map[rune]rune{}
		del := map[rune]bool{}
		for i, c := range src {
			if i < len(dst) {
				m[c] = dst[i]
			} else {
				del[c] = true
			}
		}
		var b strings.Builder
		for _, c := range args[0].text {
			if del[c] {
				continue
			}
			if rv, ok := m[c]; ok {
				b.WriteRune(rv)
			} else {
				b.WriteRune(c)
			}
		}
		return value{text: b.String(), oid: OIDText}, nil

	case "initcap":
		if len(args) < 1 || args[0].null {
			return value{null: true, oid: OIDText}, nil
		}
		words := strings.Fields(args[0].text)
		for i, w := range words {
			if len(w) > 0 {
				words[i] = strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
			}
		}
		return value{text: strings.Join(words, " "), oid: OIDText}, nil

	case "left":
		if len(args) < 2 || args[0].null {
			return value{null: true, oid: OIDText}, nil
		}
		n, _ := strconv.Atoi(args[1].text)
		rr := []rune(args[0].text)
		if n < 0 {
			n = len(rr) + n
		}
		if n < 0 {
			n = 0
		}
		if n > len(rr) {
			n = len(rr)
		}
		return value{text: string(rr[:n]), oid: OIDText}, nil

	case "right":
		if len(args) < 2 || args[0].null {
			return value{null: true, oid: OIDText}, nil
		}
		n, _ := strconv.Atoi(args[1].text)
		rr := []rune(args[0].text)
		if n < 0 {
			n = len(rr) + n
		}
		if n < 0 {
			n = 0
		}
		if n > len(rr) {
			n = len(rr)
		}
		return value{text: string(rr[len(rr)-n:]), oid: OIDText}, nil

	case "ascii":
		if len(args) < 1 || args[0].null || len(args[0].text) == 0 {
			return value{null: true, oid: OIDInt4}, nil
		}
		return value{text: strconv.Itoa(int([]rune(args[0].text)[0])), oid: OIDInt4}, nil

	case "chr":
		if len(args) < 1 || args[0].null {
			return value{null: true, oid: OIDText}, nil
		}
		n, _ := strconv.Atoi(args[0].text)
		return value{text: string(rune(n)), oid: OIDText}, nil

	case "quote_ident":
		if len(args) < 1 || args[0].null {
			return value{null: true, oid: OIDText}, nil
		}
		return value{text: `"` + strings.ReplaceAll(args[0].text, `"`, `""`) + `"`, oid: OIDText}, nil

	case "quote_literal", "quote_nullable":
		if len(args) < 1 {
			return value{null: true, oid: OIDText}, nil
		}
		if args[0].null {
			return value{text: "NULL", oid: OIDText}, nil
		}
		return value{text: `'` + strings.ReplaceAll(args[0].text, `'`, `''`) + `'`, oid: OIDText}, nil

	case "overlay":
		if len(args) < 3 || args[0].null {
			return value{null: true, oid: OIDText}, nil
		}
		s := []rune(args[0].text)
		repl := []rune(args[1].text)
		start, _ := strconv.Atoi(args[2].text)
		start--
		if start < 0 {
			start = 0
		}
		ln := len(repl)
		if len(args) >= 4 {
			ln, _ = strconv.Atoi(args[3].text)
		}
		end := start + ln
		if end > len(s) {
			end = len(s)
		}
		result := append(append(s[:start:start], repl...), s[end:]...)
		return value{text: string(result), oid: OIDText}, nil

	case "octet_length":
		if len(args) < 1 || args[0].null {
			return value{null: true, oid: OIDInt4}, nil
		}
		return value{text: strconv.Itoa(len(args[0].text)), oid: OIDInt4}, nil

	case "bit_length":
		if len(args) < 1 || args[0].null {
			return value{null: true, oid: OIDInt4}, nil
		}
		return value{text: strconv.Itoa(len(args[0].text) * 8), oid: OIDInt4}, nil

	// ── Math functions ─────────────────────────────────────────────────
	case "pi":
		return value{text: "3.141592653589793", oid: OIDFloat8}, nil

	case "power", "pow":
		if len(args) < 2 {
			return value{null: true, oid: OIDFloat8}, nil
		}
		if args[0].null || args[1].null {
			return value{null: true, oid: OIDFloat8}, nil
		}
		a, _ := strconv.ParseFloat(args[0].text, 64)
		bb, _ := strconv.ParseFloat(args[1].text, 64)
		return value{text: strconv.FormatFloat(math.Pow(a, bb), 'g', -1, 64), oid: OIDFloat8}, nil

	case "log", "log10":
		return mathFn1(args, math.Log10)

	case "ln":
		return mathFn1(args, math.Log)

	case "exp":
		return mathFn1(args, math.Exp)

	case "sign":
		if len(args) < 1 || args[0].null {
			return value{null: true, oid: OIDFloat8}, nil
		}
		v, _ := strconv.ParseFloat(args[0].text, 64)
		switch {
		case v < 0:
			return value{text: "-1", oid: OIDInt4}, nil
		case v > 0:
			return value{text: "1", oid: OIDInt4}, nil
		}
		return value{text: "0", oid: OIDInt4}, nil

	case "trunc":
		return mathFn1(args, math.Trunc)

	case "div":
		if len(args) < 2 || args[0].null || args[1].null {
			return value{null: true, oid: OIDInt8}, nil
		}
		a, _ := strconv.ParseFloat(args[0].text, 64)
		bb, _ := strconv.ParseFloat(args[1].text, 64)
		if bb == 0 {
			return value{}, newExecError("22012", "division by zero")
		}
		return value{text: strconv.FormatInt(int64(a/bb), 10), oid: OIDInt8}, nil

	case "random":
		return value{text: "0.5", oid: OIDFloat8}, nil // deterministic stub

	case "setseed":
		return value{null: true, oid: OIDUnknown}, nil

	// ── Comparison helpers ─────────────────────────────────────────────
	case "greatest":
		if len(args) == 0 {
			return value{null: true, oid: OIDUnknown}, nil
		}
		best := args[0]
		for _, a := range args[1:] {
			if a.null {
				continue
			}
			if best.null {
				best = a
				continue
			}
			if cmp, _ := compare(">", a, best); asBool(cmp) {
				best = a
			}
		}
		return best, nil

	case "least":
		if len(args) == 0 {
			return value{null: true, oid: OIDUnknown}, nil
		}
		best := args[0]
		for _, a := range args[1:] {
			if a.null {
				continue
			}
			if best.null {
				best = a
				continue
			}
			if cmp, _ := compare("<", a, best); asBool(cmp) {
				best = a
			}
		}
		return best, nil

	// ── Advisory locks (no-op stubs) ───────────────────────────────────
	case "pg_advisory_lock", "pg_advisory_lock_shared":
		return value{null: true, oid: OIDUnknown}, nil

	case "pg_advisory_unlock", "pg_advisory_unlock_shared", "pg_advisory_unlock_all":
		return value{text: "t", oid: OIDBool}, nil

	case "pg_try_advisory_lock", "pg_try_advisory_lock_shared":
		return value{text: "t", oid: OIDBool}, nil

	case "pg_sleep":
		return value{null: true, oid: OIDUnknown}, nil

	case "pg_typeof":
		if len(args) < 1 {
			return value{null: true, oid: OIDText}, nil
		}
		return value{text: sqlTypeName(args[0].oid), oid: OIDText}, nil

	// ── Sequence functions ─────────────────────────────────────────────
	case "nextval":
		if len(args) < 1 {
			return value{}, newExecError("42883", "nextval requires 1 argument")
		}
		nm := strings.Trim(args[0].text, `"'`)
		if idx := strings.LastIndex(nm, "."); idx >= 0 {
			nm = nm[idx+1:]
		}
		return value{text: seqValStr(SeqNextVal(nm)), oid: OIDInt8}, nil

	case "currval":
		if len(args) < 1 {
			return value{}, newExecError("42883", "currval requires 1 argument")
		}
		nm := strings.Trim(args[0].text, `"'`)
		if idx := strings.LastIndex(nm, "."); idx >= 0 {
			nm = nm[idx+1:]
		}
		v, err := SeqCurrVal(nm)
		if err != nil {
			return value{}, newExecError("55000", "%v", err)
		}
		return value{text: seqValStr(v), oid: OIDInt8}, nil

	case "setval":
		if len(args) < 2 {
			return value{}, newExecError("42883", "setval requires 2 arguments")
		}
		nm := strings.Trim(args[0].text, `"'`)
		if idx := strings.LastIndex(nm, "."); idx >= 0 {
			nm = nm[idx+1:]
		}
		v, e2 := strconv.ParseInt(args[1].text, 10, 64)
		if e2 != nil {
			return value{}, newExecError("22P02", "invalid value for setval")
		}
		return value{text: seqValStr(SeqSetVal(nm, v)), oid: OIDInt8}, nil

	case "lastval":
		return value{text: "1", oid: OIDInt8}, nil

	case "pg_get_serial_sequence":
		return value{null: true, oid: OIDText}, nil

	// ── Size/privilege stubs ───────────────────────────────────────────
	case "pg_relation_size", "pg_table_size", "pg_total_relation_size", "pg_database_size":
		return value{text: "0", oid: OIDInt8}, nil

	case "pg_size_pretty":
		return value{text: "0 bytes", oid: OIDText}, nil

	case "has_table_privilege", "has_schema_privilege", "has_column_privilege", "has_sequence_privilege", "pg_has_role":
		return value{text: "t", oid: OIDBool}, nil

	// ── Array functions ────────────────────────────────────────────────
	case "array_length", "cardinality":
		if len(args) < 1 || args[0].null {
			return value{null: true, oid: OIDInt4}, nil
		}
		return value{text: strconv.Itoa(len(pgArrayElements(args[0].text))), oid: OIDInt4}, nil

	case "array_position":
		// array_position(anyarray, element) → 1-based index of first match, NULL if absent.
		if len(args) < 2 || args[0].null {
			return value{null: true, oid: OIDInt4}, nil
		}
		elems := pgArrayElements(args[0].text)
		for i, el := range elems {
			if args[1].null {
				if el == "NULL" {
					return value{text: strconv.Itoa(i + 1), oid: OIDInt4}, nil
				}
				continue
			}
			if el == args[1].text {
				return value{text: strconv.Itoa(i + 1), oid: OIDInt4}, nil
			}
		}
		return value{null: true, oid: OIDInt4}, nil

	case "array_append":
		if len(args) < 2 || args[0].null {
			return value{null: true, oid: OIDTextArr}, nil
		}
		elems := pgArrayElements(args[0].text)
		if !args[1].null {
			elems = append(elems, args[1].text)
		}
		return value{text: "{" + strings.Join(elems, ",") + "}", oid: OIDTextArr}, nil

	case "array_prepend":
		if len(args) < 2 {
			return value{null: true, oid: OIDTextArr}, nil
		}
		elems := pgArrayElements(args[1].text)
		if !args[0].null {
			elems = append([]string{args[0].text}, elems...)
		}
		return value{text: "{" + strings.Join(elems, ",") + "}", oid: OIDTextArr}, nil

	case "unnest":
		// Scalar-context fallback: return the array's elements as a PG array.
		// True per-row expansion is handled in FROM / SELECT-list projection.
		if len(args) < 1 || args[0].null {
			return value{null: true, oid: OIDText}, nil
		}
		return value{text: args[0].text, oid: args[0].oid}, nil

	// ── Format ─────────────────────────────────────────────────────────
	case "format":
		if len(args) < 1 || args[0].null {
			return value{null: true, oid: OIDText}, nil
		}
		tmpl := args[0].text
		argIdx := 1
		var b strings.Builder
		for i := 0; i < len(tmpl); i++ {
			if tmpl[i] == '%' && i+1 < len(tmpl) {
				switch tmpl[i+1] {
				case 's', 'I', 'L':
					if argIdx < len(args) {
						b.WriteString(args[argIdx].text)
						argIdx++
					}
					i++
				case '%':
					b.WriteByte('%')
					i++
				default:
					b.WriteByte(tmpl[i])
				}
			} else {
				b.WriteByte(tmpl[i])
			}
		}
		return value{text: b.String(), oid: OIDText}, nil

	case "pg_encoding_to_char":
		return value{text: "UTF8", oid: OIDText}, nil

	case "num_nulls":
		n := 0
		for _, a := range args {
			if a.null {
				n++
			}
		}
		return value{text: strconv.Itoa(n), oid: OIDInt4}, nil

	case "num_nonnulls":
		n := 0
		for _, a := range args {
			if !a.null {
				n++
			}
		}
		return value{text: strconv.Itoa(n), oid: OIDInt4}, nil

	case "int4", "int8", "int2", "float4", "float8", "numeric", "bool":
		if len(args) < 1 || args[0].null {
			return value{null: true, oid: oidForTypeName(name)}, nil
		}
		return value{text: args[0].text, oid: oidForTypeName(name)}, nil

	// ── JSON modifiers ─────────────────────────────────────────────────
	case "jsonb_set", "jsonb_set_lax":
		// jsonb_set(target jsonb, path text[], new_value jsonb [, create_missing bool])
		if len(args) < 3 {
			return value{}, newExecError("42883", "jsonb_set requires at least 3 arguments")
		}
		if args[0].null || args[1].null {
			return value{null: true, oid: OIDJSONB}, nil
		}
		createMissing := true
		if len(args) >= 4 && !args[3].null {
			createMissing = args[3].text == "t" || args[3].text == "true"
		}
		return jsonbSet(args[0], args[1].text, args[2], createMissing)

	case "jsonb_array_elements", "json_array_elements",
		"jsonb_array_elements_text", "json_array_elements_text":
		// Scalar-context fallback: return the array of elements as a PG array so
		// the call does not error outside a set-returning position. True row
		// expansion is handled in FROM / SELECT-list projection.
		if len(args) < 1 || args[0].null {
			return value{null: true, oid: OIDJSONB}, nil
		}
		asText := strings.HasSuffix(name, "_text")
		elems, err := jsonbArrayElements(args[0], asText)
		if err != nil {
			return value{}, err
		}
		parts := make([]string, len(elems))
		for i, e := range elems {
			parts[i] = e.text
		}
		return value{text: "{" + strings.Join(parts, ",") + "}", oid: OIDTextArr}, nil

	// ── JSON builders ──────────────────────────────────────────────────
	case "json_build_object", "jsonb_build_object":
		if len(args)%2 != 0 {
			return value{}, newExecError("22023", "json_build_object requires even number of arguments")
		}
		var b strings.Builder
		b.WriteString("{")
		for i := 0; i < len(args); i += 2 {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`"` + strings.ReplaceAll(args[i].text, `"`, `\"`) + `":`)
			if args[i+1].null {
				b.WriteString("null")
			} else {
				b.WriteString(`"` + strings.ReplaceAll(args[i+1].text, `"`, `\"`) + `"`)
			}
		}
		b.WriteString("}")
		return value{text: b.String(), oid: OIDJSONB}, nil

	case "json_build_array", "jsonb_build_array":
		var b strings.Builder
		b.WriteString("[")
		for i, a := range args {
			if i > 0 {
				b.WriteString(",")
			}
			if a.null {
				b.WriteString("null")
			} else {
				b.WriteString(`"` + strings.ReplaceAll(a.text, `"`, `\"`) + `"`)
			}
		}
		b.WriteString("]")
		return value{text: b.String(), oid: OIDJSONB}, nil

	case "row_to_json", "to_json", "to_jsonb":
		if len(args) < 1 || args[0].null {
			return value{null: true, oid: OIDJSONB}, nil
		}
		s := args[0].text
		if !strings.HasPrefix(s, "{") && !strings.HasPrefix(s, "[") {
			s = `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
		}
		return value{text: s, oid: OIDJSONB}, nil

	// ── Bytea functions ────────────────────────────────────────────────
	case "bytea":
		if len(args) < 1 || args[0].null {
			return value{null: true, oid: OIDBytea}, nil
		}
		return value{text: args[0].text, oid: OIDBytea}, nil

	case "get_byte":
		if len(args) < 2 || args[0].null {
			return value{null: true, oid: OIDInt4}, nil
		}
		b := byteaFromText(args[0].text)
		n, _ := strconv.Atoi(args[1].text)
		if n < 0 || n >= len(b) {
			return value{}, newExecError("22P02", "index %d out of range", n)
		}
		return value{text: strconv.Itoa(int(b[n])), oid: OIDInt4}, nil

	case "set_byte":
		if len(args) < 3 || args[0].null {
			return value{null: true, oid: OIDBytea}, nil
		}
		b := byteaFromText(args[0].text)
		n, _ := strconv.Atoi(args[1].text)
		v, _ := strconv.Atoi(args[2].text)
		if n >= 0 && n < len(b) {
			b[n] = byte(v)
		}
		return value{text: byteaToText(b), oid: OIDBytea}, nil

	case "encode":
		if len(args) < 2 || args[0].null {
			return value{null: true, oid: OIDText}, nil
		}
		b := byteaFromText(args[0].text)
		switch strings.ToLower(args[1].text) {
		case "hex":
			return value{text: hex.EncodeToString(b), oid: OIDText}, nil
		case "base64":
			return value{text: base64.StdEncoding.EncodeToString(b), oid: OIDText}, nil
		default:
			return value{text: string(b), oid: OIDText}, nil
		}

	case "decode":
		if len(args) < 2 || args[0].null {
			return value{null: true, oid: OIDBytea}, nil
		}
		switch strings.ToLower(args[1].text) {
		case "hex":
			b, err := hex.DecodeString(args[0].text)
			if err != nil {
				return value{}, newExecError("22P02", "invalid hex data for decode: %v", err)
			}
			return value{text: byteaToText(b), oid: OIDBytea}, nil
		case "base64":
			b, err := base64.StdEncoding.DecodeString(args[0].text)
			if err != nil {
				return value{}, newExecError("22P02", "invalid base64 data for decode: %v", err)
			}
			return value{text: byteaToText(b), oid: OIDBytea}, nil
		default:
			return value{text: args[0].text, oid: OIDBytea}, nil
		}

	// ── Interval functions ─────────────────────────────────────────────
	case "interval":
		if len(args) < 1 || args[0].null {
			return value{null: true, oid: OIDInterval}, nil
		}
		return value{text: args[0].text, oid: OIDInterval}, nil

	case "make_interval":
		// make_interval(years, months, weeks, days, hours, mins, secs)
		// Each positional argument corresponds to a unit.
		mults := []float64{
			365 * 24 * 3600,  // years → seconds
			30 * 24 * 3600,   // months → seconds
			7 * 24 * 3600,    // weeks → seconds
			24 * 3600,        // days → seconds
			3600,             // hours → seconds
			60,               // minutes → seconds
			1,                // seconds
		}
		var total time.Duration
		for i, a := range args {
			if i >= len(mults) || a.null {
				continue
			}
			v, _ := strconv.ParseFloat(a.text, 64)
			total += time.Duration(v * mults[i] * float64(time.Second))
		}
		return value{text: formatInterval(total), oid: OIDInterval}, nil

	default:
		return value{}, newExecError("42883", "function %q does not exist", name)
	}
}

// evalArgs evaluates all arguments of a function call.
func (ev *evaluator) evalArgs(nodes []ast.Node) ([]value, error) {
	out := make([]value, len(nodes))
	for i, n := range nodes {
		v, err := ev.eval(n)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func strFn(args []value, fn func(string) string) (value, error) {
	if len(args) != 1 {
		return value{}, newExecError("42883", "string function requires 1 argument")
	}
	if args[0].null {
		return value{null: true, oid: OIDText}, nil
	}
	return value{text: fn(args[0].text), oid: OIDText}, nil
}

func mathFn1(args []value, fn func(float64) float64) (value, error) {
	if len(args) != 1 {
		return value{}, newExecError("42883", "math function requires 1 argument")
	}
	if args[0].null {
		return value{null: true, oid: OIDFloat8}, nil
	}
	x, err := strconv.ParseFloat(args[0].text, 64)
	if err != nil {
		return value{}, numErr(args[0].text)
	}
	r := fn(x)
	// Preserve integer-ness for integer inputs to abs/ceil/floor/round.
	if r == math.Trunc(r) && args[0].oid != OIDFloat8 && args[0].oid != OIDNumeric {
		return value{text: strconv.FormatInt(int64(r), 10), oid: OIDInt8}, nil
	}
	return value{text: strconv.FormatFloat(r, 'g', -1, 64), oid: OIDFloat8}, nil
}

// evalCase evaluates a CASE expression (searched or simple form), returning the
// first matching arm's result, the ELSE result, or NULL.
func (ev *evaluator) evalCase(c *ast.CaseExpr) (value, error) {
	var arg *value
	if c.Arg != nil {
		v, err := ev.eval(c.Arg)
		if err != nil {
			return value{}, err
		}
		arg = &v
	}
	for _, w := range c.Whens {
		cv, err := ev.eval(w.Cond)
		if err != nil {
			return value{}, err
		}
		matched := false
		if arg == nil {
			matched = asBool(cv) // searched CASE: Cond is a predicate
		} else if !arg.null && !cv.null { // simple CASE: arg = value
			eq, err := compare("=", *arg, cv)
			if err != nil {
				return value{}, err
			}
			matched = asBool(eq)
		}
		if matched {
			return ev.eval(w.Result)
		}
	}
	if c.Else != nil {
		return ev.eval(c.Else)
	}
	return value{null: true, oid: OIDUnknown}, nil
}

// nowText returns the current UTC time in PostgreSQL text format.
func nowText() string {
	return time.Now().UTC().Format("2006-01-02 15:04:05.999999")
}

// nowTimeText returns the current time-of-day (timetz) in PostgreSQL text form,
// e.g. "14:30:05.123456+00".
func nowTimeText() string {
	return time.Now().UTC().Format("15:04:05.999999") + "+00"
}

func negate(v value) (value, error) {
	if v.oid == OIDFloat8 {
		f, err := strconv.ParseFloat(v.text, 64)
		if err != nil {
			return value{}, newExecError("22P02", "invalid float %q", v.text)
		}
		return value{text: strconv.FormatFloat(-f, 'g', -1, 64), oid: OIDFloat8}, nil
	}
	i, err := strconv.ParseInt(v.text, 10, 64)
	if err != nil {
		return value{}, newExecError("22P02", "invalid integer %q", v.text)
	}
	return value{text: strconv.FormatInt(-i, 10), oid: OIDInt4}, nil
}

// arith computes numeric arithmetic. If either operand is float, the result is
// float8; otherwise int4 (int8 on overflow is out of milestone-1 scope).
func arith(op string, a, b value) (value, error) {
	// Timestamp ± interval arithmetic
	if (a.oid == OIDTimestamp || a.oid == OIDTimestamptz || a.oid == OIDDate) &&
		(b.oid == OIDInterval || (b.oid == OIDText && looksLikeInterval(b.text))) {
		t, err := parseTimestamp(a.text)
		if err != nil {
			return value{text: a.text, oid: a.oid}, nil
		}
		d, err := parseIntervalText(b.text)
		if err != nil {
			return value{text: a.text, oid: a.oid}, nil
		}
		switch op {
		case "+":
			return value{text: t.Add(d).Format("2006-01-02 15:04:05"), oid: OIDTimestamptz}, nil
		case "-":
			return value{text: t.Add(-d).Format("2006-01-02 15:04:05"), oid: OIDTimestamptz}, nil
		}
	}
	// Interval ± interval arithmetic
	if a.oid == OIDInterval && b.oid == OIDInterval {
		d1, _ := parseIntervalText(a.text)
		d2, _ := parseIntervalText(b.text)
		switch op {
		case "+":
			return value{text: formatDuration(d1 + d2), oid: OIDInterval}, nil
		case "-":
			return value{text: formatDuration(d1 - d2), oid: OIDInterval}, nil
		}
	}
	// NUMERIC/DECIMAL arithmetic — use math/big.Float at 256-bit precision
	// (~77 decimal digits) to avoid the float64 rounding errors that make
	// 0.1+0.2 != 0.3 and lose significance on 18+ digit decimals.
	if a.oid == OIDNumeric || b.oid == OIDNumeric {
		return numericArith(op, a.text, b.text)
	}
	if a.oid == OIDFloat8 || b.oid == OIDFloat8 {
		af, err := strconv.ParseFloat(a.text, 64)
		if err != nil {
			return value{}, numErr(a.text)
		}
		bf, err := strconv.ParseFloat(b.text, 64)
		if err != nil {
			return value{}, numErr(b.text)
		}
		var r float64
		switch op {
		case "+":
			r = af + bf
		case "-":
			r = af - bf
		case "*":
			r = af * bf
		case "/":
			if bf == 0 {
				return value{}, newExecError("22012", "division by zero")
			}
			r = af / bf
		default:
			return value{}, newExecError("0A000", "operator %q on float not supported", op)
		}
		return value{text: strconv.FormatFloat(r, 'g', -1, 64), oid: OIDFloat8}, nil
	}

	ai, err := strconv.ParseInt(a.text, 10, 64)
	if err != nil {
		return value{}, numErr(a.text)
	}
	bi, err := strconv.ParseInt(b.text, 10, 64)
	if err != nil {
		return value{}, numErr(b.text)
	}
	var r int64
	switch op {
	case "+":
		r = ai + bi
	case "-":
		r = ai - bi
	case "*":
		r = ai * bi
	case "/":
		if bi == 0 {
			return value{}, newExecError("22012", "division by zero")
		}
		r = ai / bi
	case "%":
		if bi == 0 {
			return value{}, newExecError("22012", "division by zero")
		}
		r = ai % bi
	}
	return value{text: strconv.FormatInt(r, 10), oid: OIDInt4}, nil
}

// compare evaluates a comparison and returns a bool value.
func compare(op string, a, b value) (value, error) {
	var cmp int
	// Numeric comparison: use big.Float for NUMERIC to avoid float64 precision
	// loss; use float64 for the other numeric OIDs (int/float).
	if isNumericOID(a.oid) && isNumericOID(b.oid) {
		if a.oid == OIDNumeric || b.oid == OIDNumeric {
			// At least one side is NUMERIC — use big.Float for exact comparison.
			af := parseBigFloat(a.text)
			bf := parseBigFloat(b.text)
			cmp = af.Cmp(bf)
		} else {
			af, _ := strconv.ParseFloat(a.text, 64)
			bf, _ := strconv.ParseFloat(b.text, 64)
			switch {
			case af < bf:
				cmp = -1
			case af > bf:
				cmp = 1
			}
		}
	} else {
		cmp = strings.Compare(a.text, b.text)
	}
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
	t := "f"
	if res {
		t = "t"
	}
	return value{text: t, oid: OIDBool}, nil
}

// numericPrecision is the big.Float mantissa bits used for NUMERIC arithmetic.
// 256 bits ≈ 77 decimal digits — far beyond PostgreSQL's practical precision limit
// (131072 digits before the decimal point, 16383 after).
const numericPrecision = 256

// parseBigFloat parses s into a big.Float at numericPrecision.
// Returns 0 on parse failure (non-panicking) so callers never crash.
func parseBigFloat(s string) *big.Float {
	f := new(big.Float).SetPrec(numericPrecision)
	if _, _, err := f.Parse(s, 10); err != nil {
		f.SetInt64(0)
	}
	return f
}

// formatBigFloat converts a big.Float to its minimal decimal string, stripping
// trailing zeros (e.g. "1.50000" → "1.5", "2.00000" → "2").
func formatBigFloat(f *big.Float) string {
	// 'f' with 40 decimal places gives more than enough precision.
	s := f.Text('f', 40)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

// numericArith performs NUMERIC/DECIMAL arithmetic using math/big.Float so that
// results are exact (within 77 decimal digits) and immune to float64 rounding.
func numericArith(op, atxt, btxt string) (value, error) {
	af := parseBigFloat(atxt)
	bf := parseBigFloat(btxt)
	result := new(big.Float).SetPrec(numericPrecision)
	switch op {
	case "+":
		result.Add(af, bf)
	case "-":
		result.Sub(af, bf)
	case "*":
		result.Mul(af, bf)
	case "/":
		if bf.Sign() == 0 {
			return value{}, newExecError("22012", "division by zero")
		}
		result.Quo(af, bf)
	case "%":
		// big.Float has no Mod; compute a - trunc(a/b)*b
		if bf.Sign() == 0 {
			return value{}, newExecError("22012", "division by zero")
		}
		q := new(big.Float).SetPrec(numericPrecision).Quo(af, bf)
		qi, _ := q.Int(nil) // truncate toward zero
		qf := new(big.Float).SetPrec(numericPrecision).SetInt(qi)
		rem := new(big.Float).SetPrec(numericPrecision).Mul(qf, bf)
		result.Sub(af, rem)
	default:
		return value{}, newExecError("0A000", "operator %q not supported for numeric", op)
	}
	return value{text: formatBigFloat(result), oid: OIDNumeric}, nil
}

func isNumericOID(oid uint32) bool {
	return oid == OIDInt4 || oid == OIDInt8 || oid == OIDFloat8 ||
		oid == OIDInt2 || oid == OIDFloat4 || oid == OIDNumeric
}

func numErr(s string) error { return newExecError("22P02", "invalid numeric value %q", s) }

func oidForTypeName(name string) uint32 {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "int", "int4", "integer", "serial":
		return OIDInt4
	case "bigint", "int8", "bigserial":
		return OIDInt8
	case "smallint", "int2", "smallserial":
		return OIDInt2
	case "real", "float4":
		return OIDFloat4
	case "float8", "double precision", "float":
		return OIDFloat8
	case "numeric", "decimal":
		return OIDNumeric
	case "bool", "boolean":
		return OIDBool
	case "uuid":
		return OIDUUID
	case "json":
		return OIDJSON
	case "jsonb":
		return OIDJSONB
	case "bytea":
		return OIDBytea
	case "date":
		return OIDDate
	case "time", "time without time zone", "time with time zone", "timetz":
		return OIDTime
	case "timestamp", "timestamp without time zone":
		return OIDTimestamp
	case "timestamptz", "timestamp with time zone":
		return OIDTimestamptz
	case "varchar", "character varying":
		return OIDVarchar
	case "char", "character", "bpchar":
		return OIDBpchar
	case "text":
		return OIDText
	case "interval":
		return OIDInterval
	// Array types
	case "text[]", "varchar[]", "character varying[]":
		return OIDTextArr
	case "int[]", "int4[]", "integer[]":
		return OIDInt4Arr
	case "int8[]", "bigint[]":
		return OIDInt8Arr
	case "bool[]", "boolean[]":
		return OIDBoolArr
	case "float8[]", "double precision[]":
		return OIDFloat8Arr
	case "uuid[]":
		return 2951 // uuidarray
	case "jsonb[]":
		return 3807 // jsonbarr
	case "json[]":
		return 199 // jsonarr

	// Range and multirange types — all fall back to text representation.
	case "int4range", "int8range", "numrange", "tsrange", "tstzrange", "daterange",
		"int4multirange", "int8multirange", "nummultirange", "tsmultirange", "tstzmultirange", "datemultirange":
		return 3904 // int4range OID (approximate — all range types use text fallback)

	// Network address types.
	case "inet":
		return 869
	case "cidr":
		return 650
	case "macaddr":
		return 829
	case "macaddr8":
		return 774

	// Full-text search types.
	case "tsvector":
		return 3614
	case "tsquery":
		return 3615

	// Monetary type.
	case "money":
		return 790

	// XML type.
	case "xml":
		return 142

	// JSON path.
	case "jsonpath":
		return 4072

	// Bit string types.
	case "bit", "varbit", "bit varying":
		return 1560

	// Geometric types.
	case "point":
		return 600
	case "line":
		return 628
	case "lseg":
		return 601
	case "box":
		return 603
	case "path":
		return 602
	case "polygon":
		return 604
	case "circle":
		return 718

	// System types.
	case "txid_snapshot", "pg_snapshot":
		return 2970
	case "pg_lsn":
		return 3220
	case "void":
		return 2278
	case "cstring":
		return 2275
	case "name":
		return 19
	case "xid":
		return 28
	case "cid":
		return 29
	case "tid":
		return 27

	default:
		return OIDText
	}
}

var _ = fmt.Sprintf

// pgLike implements SQL LIKE pattern matching (% = any sequence, _ = any char).
func pgLike(s, pattern string) bool {
	var re strings.Builder
	re.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '%':
			re.WriteString(".*")
		case '_':
			re.WriteString(".")
		case '.', '+', '(', ')', '[', ']', '{', '}', '\\', '^', '$', '|', '?', '*':
			re.WriteString("\\")
			re.WriteByte(pattern[i])
		default:
			re.WriteByte(pattern[i])
		}
	}
	re.WriteString("$")
	matched, _ := regexp.MatchString(re.String(), s)
	return matched
}

// pgILike is case-insensitive LIKE.
func pgILike(s, pattern string) bool {
	return pgLike(strings.ToLower(s), strings.ToLower(pattern))
}

// similarToRegex converts a SQL SIMILAR TO pattern to a Go regexp.
// SQL SIMILAR TO: % = .*, _ = ., | = |, () = (), other metacharacters are literal.
func similarToRegex(pattern string) string {
	var re strings.Builder
	re.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '%':
			re.WriteString(".*")
		case '_':
			re.WriteString(".")
		case '.', '+', '\\', '^', '$', '{', '}', '[', ']':
			re.WriteByte('\\')
			re.WriteByte(pattern[i])
		default:
			re.WriteByte(pattern[i])
		}
	}
	re.WriteString("$")
	return re.String()
}

// dateTrunc truncates a timestamp to the specified precision.
func dateTrunc(field, ts string) (value, error) {
	t, err := parseTimestamp(ts)
	if err != nil {
		return value{null: true, oid: OIDTimestamptz}, nil
	}
	switch strings.ToLower(strings.Trim(field, `"' `)) {
	case "microseconds", "milliseconds", "second":
		t = t.Truncate(time.Second)
	case "minute":
		t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
	case "hour":
		t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC)
	case "day":
		t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	case "week":
		wd := int(t.Weekday())
		if wd == 0 {
			wd = 7
		}
		t = time.Date(t.Year(), t.Month(), t.Day()-wd+1, 0, 0, 0, 0, time.UTC)
	case "month":
		t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	case "quarter":
		m := ((int(t.Month())-1)/3)*3 + 1
		t = time.Date(t.Year(), time.Month(m), 1, 0, 0, 0, 0, time.UTC)
	case "year":
		t = time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	case "decade":
		t = time.Date((t.Year()/10)*10, 1, 1, 0, 0, 0, 0, time.UTC)
	case "century":
		t = time.Date(((t.Year()-1)/100)*100+1, 1, 1, 0, 0, 0, 0, time.UTC)
	case "millennium":
		t = time.Date(((t.Year()-1)/1000)*1000+1, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return value{text: t.Format("2006-01-02 15:04:05"), oid: OIDTimestamptz}, nil
}

// datePart extracts a date/time field from a timestamp.
func datePart(field, ts string) (value, error) {
	t, err := parseTimestamp(ts)
	if err != nil {
		return value{text: "0", oid: OIDFloat8}, nil
	}
	var r float64
	switch strings.ToLower(strings.Trim(field, `"' `)) {
	case "epoch":
		r = float64(t.Unix())
	case "year":
		r = float64(t.Year())
	case "month":
		r = float64(t.Month())
	case "day":
		r = float64(t.Day())
	case "hour":
		r = float64(t.Hour())
	case "minute":
		r = float64(t.Minute())
	case "second":
		r = float64(t.Second())
	case "dow":
		r = float64(t.Weekday())
	case "doy":
		r = float64(t.YearDay())
	case "week":
		_, w := t.ISOWeek()
		r = float64(w)
	case "quarter":
		r = float64((int(t.Month())-1)/3 + 1)
	case "decade":
		r = float64(t.Year() / 10)
	case "century":
		r = float64((t.Year() + 99) / 100)
	case "milliseconds":
		r = float64(t.Second()*1000 + t.Nanosecond()/1000000)
	case "microseconds":
		r = float64(t.Second()*1000000 + t.Nanosecond()/1000)
	default:
		r = 0
	}
	return value{text: strconv.FormatFloat(r, 'f', -1, 64), oid: OIDFloat8}, nil
}

// parseTimestamp parses a timestamp string in common PG formats.
func parseTimestamp(s string) (time.Time, error) {
	for _, f := range []string{
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05.999999Z",
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
		"2006-01-02",
		time.RFC3339Nano, time.RFC3339,
	} {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse timestamp: %q", s)
}

// formatInterval formats a duration as a PG-style interval string.
func formatInterval(d time.Duration) string {
	if d < 0 {
		return "-" + formatInterval(-d)
	}
	days := int(d.Hours() / 24)
	h := int(d.Hours()) % 24
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if days > 0 {
		return fmt.Sprintf("%d days %02d:%02d:%02d", days, h, m, s)
	}
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

// byteaToText normalises a bytea value for storage/display.
// PostgreSQL bytea hex format: \xHHHH...
// We store as the hex string internally.
func byteaToText(raw []byte) string {
	return `\x` + hex.EncodeToString(raw)
}

func byteaFromText(s string) []byte {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, `\\x`) {
		h := s[3:]
		if b, err := hex.DecodeString(h); err == nil {
			return b
		}
	}
	if strings.HasPrefix(s, `\x`) {
		h := s[2:]
		if b, err := hex.DecodeString(h); err == nil {
			return b
		}
	}
	// Escape format: \001 etc. — return as-is for now
	return []byte(s)
}

// looksLikeInterval returns true when s looks like an interval word-form
// (e.g. "3 days", "2 hours 30 minutes") as opposed to a timestamp.
func looksLikeInterval(s string) bool {
	sl := strings.ToLower(s)
	return (strings.Contains(sl, "day") ||
		strings.Contains(sl, "hour") ||
		strings.Contains(sl, "minute") ||
		strings.Contains(sl, "second") ||
		strings.Contains(sl, "week") ||
		strings.Contains(sl, "month") ||
		strings.Contains(sl, "year")) &&
		!strings.Contains(sl, "-") // avoid matching timestamp strings
}

// parseIntervalText parses PostgreSQL interval strings like '3 days', '2 hours 30 minutes',
// '1 year', '00:30:00', etc.
func parseIntervalText(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	// Strip surrounding quotes if any
	s = strings.Trim(s, "'")
	s = strings.TrimSpace(s)

	// Try HH:MM:SS format first
	if strings.Count(s, ":") >= 1 {
		parts := strings.Split(s, ":")
		h, _ := strconv.ParseFloat(parts[0], 64)
		m := 0.0
		sec := 0.0
		if len(parts) > 1 {
			m, _ = strconv.ParseFloat(parts[1], 64)
		}
		if len(parts) > 2 {
			sec, _ = strconv.ParseFloat(parts[2], 64)
		}
		return time.Duration(h*float64(time.Hour) + m*float64(time.Minute) + sec*float64(time.Second)), nil
	}

	// Word format: "3 days 2 hours 30 minutes 10 seconds"
	var total time.Duration
	words := strings.Fields(s)
	parsed := false
	for i := 0; i+1 < len(words); i += 2 {
		v, err := strconv.ParseFloat(words[i], 64)
		if err != nil {
			// might be a unit without a preceding number — skip
			i-- // retry with same i+1 re-offset
			continue
		}
		unit := words[i+1]
		switch {
		case strings.HasPrefix(unit, "microsecond"):
			total += time.Duration(v * float64(time.Microsecond))
			parsed = true
		case strings.HasPrefix(unit, "millisecond"):
			total += time.Duration(v * float64(time.Millisecond))
			parsed = true
		case strings.HasPrefix(unit, "second"):
			total += time.Duration(v * float64(time.Second))
			parsed = true
		case strings.HasPrefix(unit, "minute"):
			total += time.Duration(v * float64(time.Minute))
			parsed = true
		case strings.HasPrefix(unit, "hour"):
			total += time.Duration(v * float64(time.Hour))
			parsed = true
		case strings.HasPrefix(unit, "day"):
			total += time.Duration(v * 24 * float64(time.Hour))
			parsed = true
		case strings.HasPrefix(unit, "week"):
			total += time.Duration(v * 7 * 24 * float64(time.Hour))
			parsed = true
		case strings.HasPrefix(unit, "month"):
			total += time.Duration(v * 30 * 24 * float64(time.Hour))
			parsed = true
		case strings.HasPrefix(unit, "year"):
			total += time.Duration(v * 365 * 24 * float64(time.Hour))
			parsed = true
		}
	}
	if !parsed && len(words) >= 1 {
		// single number without unit — try as seconds
		if n, err := strconv.ParseFloat(words[0], 64); err == nil {
			return time.Duration(n * float64(time.Second)), nil
		}
	}
	return total, nil
}

// formatDuration is an alias to formatInterval for interval arithmetic.
func formatDuration(d time.Duration) string {
	return formatInterval(d)
}

// toCharFn formats a timestamp value using a PG-style format string.
func toCharFn(v value, format string) (value, error) {
	t, err := parseTimestamp(v.text)
	if err != nil {
		return value{text: v.text, oid: OIDText}, nil
	}
	result := format
	result = strings.ReplaceAll(result, "YYYY", fmt.Sprintf("%04d", t.Year()))
	result = strings.ReplaceAll(result, "YY", fmt.Sprintf("%02d", t.Year()%100))
	result = strings.ReplaceAll(result, "MM", fmt.Sprintf("%02d", t.Month()))
	result = strings.ReplaceAll(result, "DD", fmt.Sprintf("%02d", t.Day()))
	result = strings.ReplaceAll(result, "HH24", fmt.Sprintf("%02d", t.Hour()))
	result = strings.ReplaceAll(result, "HH12", fmt.Sprintf("%02d", t.Hour()%12))
	result = strings.ReplaceAll(result, "HH", fmt.Sprintf("%02d", t.Hour()))
	result = strings.ReplaceAll(result, "MI", fmt.Sprintf("%02d", t.Minute()))
	result = strings.ReplaceAll(result, "SS", fmt.Sprintf("%02d", t.Second()))
	result = strings.ReplaceAll(result, "MS", fmt.Sprintf("%03d", t.Nanosecond()/1000000))
	result = strings.ReplaceAll(result, "TZ", "UTC")
	result = strings.ReplaceAll(result, "Month", t.Month().String())
	result = strings.ReplaceAll(result, "Mon", t.Month().String()[:3])
	return value{text: result, oid: OIDText}, nil
}
