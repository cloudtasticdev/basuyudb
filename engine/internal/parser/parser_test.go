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

// TestRowLevelSecurityDDL verifies CREATE/ALTER/DROP POLICY and ALTER TABLE ...
// ROW LEVEL SECURITY parse into the expected AST shapes.
func TestRowLevelSecurityDDL(t *testing.T) {
	n, err := Parse("CREATE POLICY p ON docs AS RESTRICTIVE FOR SELECT TO alice, bob USING (owner = current_user) WITH CHECK (owner = current_user)")
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}
	cp, ok := n.(*ast.CreatePolicyStmt)
	if !ok {
		t.Fatalf("want *CreatePolicyStmt, got %T", n)
	}
	if cp.PolicyName != "p" || cp.Table != "docs" || cp.Permissive || cp.Command != "SELECT" {
		t.Fatalf("create policy fields wrong: %#v", cp)
	}
	if len(cp.Roles) != 2 || cp.Roles[0] != "alice" || cp.Roles[1] != "bob" {
		t.Fatalf("roles wrong: %#v", cp.Roles)
	}
	if cp.Using == nil || cp.WithCheck == nil {
		t.Fatalf("using/check missing: %#v", cp)
	}

	// Default permissive + command ALL.
	n2, err := Parse("CREATE POLICY p2 ON docs USING (true)")
	if err != nil {
		t.Fatalf("create policy defaults: %v", err)
	}
	cp2 := n2.(*ast.CreatePolicyStmt)
	if !cp2.Permissive || cp2.Command != "ALL" {
		t.Fatalf("defaults wrong: %#v", cp2)
	}

	if n, err := Parse("ALTER POLICY p ON docs TO alice USING (true)"); err != nil {
		t.Fatalf("alter policy: %v", err)
	} else if _, ok := n.(*ast.AlterPolicyStmt); !ok {
		t.Fatalf("want *AlterPolicyStmt, got %T", n)
	}

	if n, err := Parse("DROP POLICY IF EXISTS p ON docs"); err != nil {
		t.Fatalf("drop policy: %v", err)
	} else if dp, ok := n.(*ast.DropPolicyStmt); !ok || !dp.IfExists || dp.PolicyName != "p" || dp.Table != "docs" {
		t.Fatalf("drop policy wrong: %#v", n)
	}

	rls := []struct {
		sql  string
		kind ast.AlterTableKind
	}{
		{"ALTER TABLE docs ENABLE ROW LEVEL SECURITY", ast.AlterEnableRLS},
		{"ALTER TABLE docs DISABLE ROW LEVEL SECURITY", ast.AlterDisableRLS},
		{"ALTER TABLE docs FORCE ROW LEVEL SECURITY", ast.AlterForceRLS},
		{"ALTER TABLE docs NO FORCE ROW LEVEL SECURITY", ast.AlterNoForceRLS},
	}
	for _, c := range rls {
		n, err := Parse(c.sql)
		if err != nil {
			t.Fatalf("%q: %v", c.sql, err)
		}
		at, ok := n.(*ast.AlterTableStmt)
		if !ok || at.Kind != c.kind {
			t.Fatalf("%q: want kind %d, got %#v", c.sql, c.kind, n)
		}
	}
}

// TestTableConstraints verifies table-level constraints in CREATE TABLE.
func TestTableConstraints(t *testing.T) {
	sql := "CREATE TABLE t (" +
		"a int, b int, " +
		"CONSTRAINT pk PRIMARY KEY (a, b), " +
		"UNIQUE (b), " +
		"FOREIGN KEY (a) REFERENCES parent (id) ON DELETE CASCADE, " +
		"CHECK (a > 0))"
	n, err := Parse(sql)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := n.(*ast.CreateStmt)
	if len(c.TableElts) != 2 {
		t.Fatalf("want 2 columns, got %d", len(c.TableElts))
	}
	if len(c.TableConstraints) != 4 {
		t.Fatalf("want 4 table constraints, got %d", len(c.TableConstraints))
	}
	pk := c.TableConstraints[0]
	if pk.ConstraintType != ast.ConstrPrimaryKey || pk.Name != "pk" || len(pk.Columns) != 2 {
		t.Fatalf("pk wrong: %#v", pk)
	}
	fk := c.TableConstraints[2]
	if fk.ConstraintType != ast.ConstrForeignKey || fk.RefTable != "parent" || fk.OnDelete != "CASCADE" {
		t.Fatalf("fk wrong: %#v", fk)
	}
}

// TestTableConstraintDeferrable verifies the trailing [NOT] DEFERRABLE /
// INITIALLY clause on table-level constraints and ALTER TABLE ADD CONSTRAINT.
func TestTableConstraintDeferrable(t *testing.T) {
	n, err := Parse("CREATE TABLE c (a int, b int, " +
		"FOREIGN KEY (a) REFERENCES p(id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED)")
	if err != nil {
		t.Fatalf("parse create: %v", err)
	}
	c := n.(*ast.CreateStmt)
	if len(c.TableConstraints) != 1 {
		t.Fatalf("want 1 table constraint, got %d", len(c.TableConstraints))
	}
	fk := c.TableConstraints[0]
	if fk.ConstraintType != ast.ConstrForeignKey || fk.RefTable != "p" || fk.OnDelete != "CASCADE" {
		t.Fatalf("fk basics wrong: %#v", fk)
	}
	if !fk.Deferrable || !fk.InitiallyDeferred {
		t.Fatalf("want Deferrable && InitiallyDeferred, got Deferrable=%v InitiallyDeferred=%v",
			fk.Deferrable, fk.InitiallyDeferred)
	}

	n2, err := Parse("ALTER TABLE c ADD CONSTRAINT fk FOREIGN KEY (a) REFERENCES p(id) DEFERRABLE")
	if err != nil {
		t.Fatalf("parse alter: %v", err)
	}
	at := n2.(*ast.AlterTableStmt)
	if len(at.Cmds) != 1 || at.Cmds[0].Constraint == nil {
		t.Fatalf("want 1 add-constraint cmd, got %#v", at.Cmds)
	}
	con := at.Cmds[0].Constraint
	if con.ConstraintType != ast.ConstrForeignKey || con.Name != "fk" || con.RefTable != "p" {
		t.Fatalf("alter fk basics wrong: %#v", con)
	}
	if !con.Deferrable || con.InitiallyDeferred {
		t.Fatalf("want Deferrable && !InitiallyDeferred, got Deferrable=%v InitiallyDeferred=%v",
			con.Deferrable, con.InitiallyDeferred)
	}
}

// TestFieldSelectAndCompositeType verifies (expr).field access and CREATE TYPE
// composite parse correctly.
func TestFieldSelectAndCompositeType(t *testing.T) {
	n, err := Parse("SELECT (ROW(1, 2)).f1")
	if err != nil {
		t.Fatalf("field select: %v", err)
	}
	sel := n.(*ast.SelectStmt)
	fs, ok := sel.TargetList[0].Val.(*ast.FieldSelect)
	if !ok || fs.Field != "f1" {
		t.Fatalf("want FieldSelect f1, got %#v", sel.TargetList[0].Val)
	}

	n2, err := Parse("CREATE TYPE pt AS (x int, y int)")
	if err != nil {
		t.Fatalf("create type composite: %v", err)
	}
	ct, ok := n2.(*ast.CreateTypeStmt)
	if !ok || ct.Name != "pt" || len(ct.Fields) != 2 {
		t.Fatalf("want CreateTypeStmt pt with 2 fields, got %#v", n2)
	}
	if ct.Fields[0].Name != "x" || ct.Fields[0].TypeName != "int" {
		t.Fatalf("field 0 wrong: %#v", ct.Fields[0])
	}

	// CREATE TYPE AS ENUM must still build CreateEnumStmt.
	n3, err := Parse("CREATE TYPE mood AS ENUM ('happy', 'sad')")
	if err != nil {
		t.Fatalf("create type enum: %v", err)
	}
	if _, ok := n3.(*ast.CreateEnumStmt); !ok {
		t.Fatalf("want CreateEnumStmt, got %T", n3)
	}
}
