package executor

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
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

	case *ast.SubLink:
		return ev.evalScalarSub(e)

	case *ast.FuncCall:
		return ev.evalFunc(e)

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
	case "+", "-", "*", "/", "%":
		return arith(e.Name, lv, rv)
	case "=", "<>", "!=", "<", ">", "<=", ">=":
		return compare(e.Name, lv, rv)
	case "->>", "->", "#>>":
		return jsonbExtract(e.Name, lv, rv)
	default:
		return value{}, newExecError("0A000", "operator %q not supported", e.Name)
	}
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
	default:
		return value{}, newExecError("42883", "function %q does not exist (milestone-1)", name)
	}
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
	// Numeric comparison when both look numeric; else lexical text comparison.
	if isNumericOID(a.oid) && isNumericOID(b.oid) {
		af, _ := strconv.ParseFloat(a.text, 64)
		bf, _ := strconv.ParseFloat(b.text, 64)
		switch {
		case af < bf:
			cmp = -1
		case af > bf:
			cmp = 1
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

func isNumericOID(oid uint32) bool { return oid == OIDInt4 || oid == OIDInt8 || oid == OIDFloat8 }

func numErr(s string) error { return newExecError("22P02", "invalid numeric value %q", s) }

func oidForTypeName(name string) uint32 {
	switch strings.ToLower(name) {
	case "int", "int4", "integer":
		return OIDInt4
	case "bigint", "int8":
		return OIDInt8
	case "float8", "double precision", "real":
		return OIDFloat8
	case "bool", "boolean":
		return OIDBool
	default:
		return OIDText
	}
}

var _ = fmt.Sprintf
