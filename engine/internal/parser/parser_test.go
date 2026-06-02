package parser

import (
	"testing"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
)

// TestSelectOne is the Gate-1 parse: SELECT 1 must yield a SelectStmt with one
// integer-constant target.
func TestSelectOne(t *testing.T) {
	n, err := Parse("SELECT 1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sel, ok := n.(*ast.SelectStmt)
	if !ok {
		t.Fatalf("want *ast.SelectStmt, got %T", n)
	}
	if len(sel.TargetList) != 1 {
		t.Fatalf("want 1 target, got %d", len(sel.TargetList))
	}
	c, ok := sel.TargetList[0].Val.(*ast.A_Const)
	if !ok || c.Type != ast.ConstInt || c.Val != "1" {
		t.Fatalf("want integer const 1, got %#v", sel.TargetList[0].Val)
	}
}

// TestOTelJoinParse is the Gate-3 parse: the OTel JOIN demo query must produce
// a JoinExpr with the JSONB ->> operator binding tighter than =.
func TestOTelJoinParse(t *testing.T) {
	sql := `SELECT s.trace_id, u.email
	 FROM otel_spans s JOIN users u ON u.id = s.attributes ->> 'user_id'
	 WHERE s.status = 'ERROR'
	 ORDER BY s.trace_id DESC
	 LIMIT 10`
	n, err := Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sel := n.(*ast.SelectStmt)

	if len(sel.FromClause) != 1 {
		t.Fatalf("want 1 from-item, got %d", len(sel.FromClause))
	}
	join, ok := sel.FromClause[0].(*ast.JoinExpr)
	if !ok {
		t.Fatalf("want *ast.JoinExpr, got %T", sel.FromClause[0])
	}
	if join.JoinType != ast.JOIN_INNER {
		t.Fatalf("want inner join, got %d", join.JoinType)
	}

	// ON u.id = (s.attributes ->> 'user_id') — the = must be the top operator
	// and its right operand must be the ->> expression (precedence check).
	on, ok := join.Quals.(*ast.A_Expr)
	if !ok || on.Name != "=" {
		t.Fatalf("want = at ON root, got %#v", join.Quals)
	}
	rhs, ok := on.Rexpr.(*ast.A_Expr)
	if !ok || rhs.Name != "->>" {
		t.Fatalf("want ->> as = right operand (precedence), got %#v", on.Rexpr)
	}
	lhsCol, ok := rhs.Lexpr.(*ast.ColumnRef)
	if !ok || len(lhsCol.Fields) != 2 || lhsCol.Fields[0] != "s" || lhsCol.Fields[1] != "attributes" {
		t.Fatalf("want s.attributes as ->> left operand, got %#v", rhs.Lexpr)
	}

	if sel.LimitCount == nil {
		t.Fatal("want LIMIT")
	}
	if len(sel.SortClause) != 1 || sel.SortClause[0].SortDir != 2 {
		t.Fatalf("want one DESC sort, got %#v", sel.SortClause)
	}
}

// TestDMLAndDDL exercises INSERT/UPDATE/DELETE/CREATE TABLE and branch DDL.
func TestDMLAndDDL(t *testing.T) {
	cases := []struct {
		sql string
		want ast.NodeTag
	}{
		{"INSERT INTO users (id, email) VALUES ($1, $2)", ast.T_InsertStmt},
		{"UPDATE users SET email = 'x@y.z' WHERE id = $1", ast.T_UpdateStmt},
		{"DELETE FROM users WHERE id = $1", ast.T_DeleteStmt},
		{"CREATE TABLE users (id text PRIMARY KEY, email text NOT NULL)", ast.T_CreateStmt},
		{"CREATE BRANCH feature FROM main", ast.T_CreateBranchStmt},
		{"MERGE BRANCH feature INTO main", ast.T_MergeBranchStmt},
		{"DROP BRANCH feature", ast.T_DropBranchStmt},
	}
	for _, c := range cases {
		n, err := Parse(c.sql)
		if err != nil {
			t.Fatalf("parse %q: %v", c.sql, err)
		}
		if ast.TagOf(n) != c.want {
			t.Fatalf("%q: want tag %d, got %d", c.sql, c.want, ast.TagOf(n))
		}
	}
}

// TestParseErrorStructured verifies a syntax error returns a ParseError with a
// PG SQLSTATE rather than panicking.
func TestParseErrorStructured(t *testing.T) {
	_, err := Parse("SELECT FROM WHERE")
	if err == nil {
		t.Fatal("expected parse error")
	}
	pe, ok := err.(*ParseError)
	if !ok {
		t.Fatalf("want *ParseError, got %T", err)
	}
	if pe.SQLSTATE == "" {
		t.Fatal("ParseError missing SQLSTATE")
	}
}

// TestCreateTableColumns verifies column defs and PRIMARY KEY parse correctly.
func TestCreateTableColumns(t *testing.T) {
	n, err := Parse("CREATE TABLE t (id text PRIMARY KEY, name text NOT NULL, age int)")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := n.(*ast.CreateStmt)
	if len(c.TableElts) != 3 {
		t.Fatalf("want 3 columns, got %d", len(c.TableElts))
	}
	if !c.TableElts[0].PrimaryKey || c.TableElts[0].ColName != "id" {
		t.Fatalf("col 0 should be id PRIMARY KEY, got %#v", c.TableElts[0])
	}
	if !c.TableElts[1].NotNull || c.TableElts[1].ColName != "name" {
		t.Fatalf("col 1 should be name NOT NULL, got %#v", c.TableElts[1])
	}
}
