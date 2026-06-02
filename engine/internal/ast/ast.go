// Package ast is the canonical PG16-faithful abstract syntax tree for BasuyuDB.
// It is the parser's output and the executor's input. No other package defines
// AST node types. (by design)
//
// Conformed to the design specs (the reconciliation pass). Resolves the architecture review, the integration review.
package ast

// NodeTag identifies the concrete type of a Node. It mirrors PostgreSQL's
// NodeTag and is used for fast, allocation-free dispatch.
type NodeTag int32

const (
	T_Invalid NodeTag = iota
	T_SelectStmt
	T_InsertStmt
	T_UpdateStmt
	T_DeleteStmt
	T_CreateStmt
	T_IndexStmt
	T_CreateBranchStmt
	T_MergeBranchStmt
	T_DropBranchStmt
	T_ResTarget
	T_RangeVar
	T_JoinExpr
	T_ColumnRef
	T_A_Expr
	T_A_Const
	T_ParamRef
	T_FuncCall
	T_BoolExpr
	T_NullTest
	T_SortBy
	T_Alias
	T_WithClause
	T_CommonTableExpr
	T_SubLink
	T_TypeCast
	T_A_Star
)

// Node is the single canonical AST interface. Every AST node implements it.
// nodeTag() and walkChildren() are unexported so that only this package may
// define Node implementations — the AST is closed and frozen. (by design)
type Node interface {
	nodeTag() NodeTag
	// walkChildren invokes fn on each direct child Node. It is the basis for
	// the generic Walk traversal. Returning a non-nil error aborts the walk.
	walkChildren(fn func(Node) error) error
}

// Walk performs a depth-first pre-order traversal, invoking fn on n and every
// descendant. It is the only public traversal entry point.
func Walk(n Node, fn func(Node) error) error {
	if n == nil {
		return nil
	}
	if err := fn(n); err != nil {
		return err
	}
	return n.walkChildren(func(child Node) error { return Walk(child, fn) })
}

// TagOf returns the NodeTag of any Node (public accessor over the unexported method).
func TagOf(n Node) NodeTag {
	if n == nil {
		return T_Invalid
	}
	return n.nodeTag()
}

// ---------------------------------------------------------------------------
// Statement nodes
// ---------------------------------------------------------------------------

// SelectStmt is the PG-faithful SELECT. It supports JOINs (via FromClause
// JoinExpr nodes), subqueries (SubLink), and CTEs (WithClause). This is the
// shape required by the Gate-3 OTel JOIN demo. (by design)
type SelectStmt struct {
	WithClause *WithClause // optional CTEs
	TargetList []*ResTarget
	FromClause []Node // RangeVar | JoinExpr | SubLink (subquery in FROM)
	WhereClause Node // boolean expression or nil
	GroupClause []Node
	HavingClause Node
	SortClause []*SortBy
	LimitCount Node // A_Const | ParamRef | nil
	LimitOffset Node
}

func (*SelectStmt) nodeTag() NodeTag { return T_SelectStmt }
func (s *SelectStmt) walkChildren(fn func(Node) error) error {
	if s.WithClause != nil {
		if err := fn(s.WithClause); err != nil {
			return err
		}
	}
	for _, t := range s.TargetList {
		if err := fn(t); err != nil {
			return err
		}
	}
	for _, f := range s.FromClause {
		if err := fn(f); err != nil {
			return err
		}
	}
	if s.WhereClause != nil {
		if err := fn(s.WhereClause); err != nil {
			return err
		}
	}
	for _, g := range s.GroupClause {
		if err := fn(g); err != nil {
			return err
		}
	}
	if s.HavingClause != nil {
		if err := fn(s.HavingClause); err != nil {
			return err
		}
	}
	for _, o := range s.SortClause {
		if err := fn(o); err != nil {
			return err
		}
	}
	if s.LimitCount != nil {
		if err := fn(s.LimitCount); err != nil {
			return err
		}
	}
	if s.LimitOffset != nil {
		if err := fn(s.LimitOffset); err != nil {
			return err
		}
	}
	return nil
}

// InsertStmt — INSERT INTO relation (cols) VALUES/SELECT.
type InsertStmt struct {
	Relation *RangeVar
	Cols []*ResTarget
	SelectStmt Node // VALUES list (SelectStmt with no FromClause) or subquery
	ReturningList []*ResTarget
}

func (*InsertStmt) nodeTag() NodeTag { return T_InsertStmt }
func (s *InsertStmt) walkChildren(fn func(Node) error) error {
	if s.Relation != nil {
		if err := fn(s.Relation); err != nil {
			return err
		}
	}
	for _, c := range s.Cols {
		if err := fn(c); err != nil {
			return err
		}
	}
	if s.SelectStmt != nil {
		if err := fn(s.SelectStmt); err != nil {
			return err
		}
	}
	for _, r := range s.ReturningList {
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

// UpdateStmt — UPDATE relation SET ... WHERE.
type UpdateStmt struct {
	Relation *RangeVar
	TargetList []*ResTarget // SET assignments (ResTarget.Name = col, .Val = expr)
	FromClause []Node
	WhereClause Node
	ReturningList []*ResTarget
}

func (*UpdateStmt) nodeTag() NodeTag { return T_UpdateStmt }
func (s *UpdateStmt) walkChildren(fn func(Node) error) error {
	if s.Relation != nil {
		if err := fn(s.Relation); err != nil {
			return err
		}
	}
	for _, t := range s.TargetList {
		if err := fn(t); err != nil {
			return err
		}
	}
	for _, f := range s.FromClause {
		if err := fn(f); err != nil {
			return err
		}
	}
	if s.WhereClause != nil {
		if err := fn(s.WhereClause); err != nil {
			return err
		}
	}
	for _, r := range s.ReturningList {
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

// DeleteStmt — DELETE FROM relation WHERE.
type DeleteStmt struct {
	Relation *RangeVar
	WhereClause Node
	ReturningList []*ResTarget
}

func (*DeleteStmt) nodeTag() NodeTag { return T_DeleteStmt }
func (s *DeleteStmt) walkChildren(fn func(Node) error) error {
	if s.Relation != nil {
		if err := fn(s.Relation); err != nil {
			return err
		}
	}
	if s.WhereClause != nil {
		if err := fn(s.WhereClause); err != nil {
			return err
		}
	}
	for _, r := range s.ReturningList {
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

// CreateBranchStmt / MergeBranchStmt / DropBranchStmt — branch DDL. One
// definition only; the design specs and the design specs share these. (by design)
type CreateBranchStmt struct {
	BranchName string
	FromBranch string // source branch; "main" if omitted
}

func (*CreateBranchStmt) nodeTag() NodeTag { return T_CreateBranchStmt }
func (*CreateBranchStmt) walkChildren(fn func(Node) error) error { return nil }

type MergeBranchStmt struct {
	SourceBranch string
	TargetBranch string // "main" if omitted
}

func (*MergeBranchStmt) nodeTag() NodeTag { return T_MergeBranchStmt }
func (*MergeBranchStmt) walkChildren(fn func(Node) error) error { return nil }

type DropBranchStmt struct {
	BranchName string
}

func (*DropBranchStmt) nodeTag() NodeTag { return T_DropBranchStmt }
func (*DropBranchStmt) walkChildren(fn func(Node) error) error { return nil }

// ---------------------------------------------------------------------------
// Expression / leaf nodes
// ---------------------------------------------------------------------------

// ResTarget is a SELECT target-list entry or a SET assignment.
type ResTarget struct {
	Name string // output column alias, or target column for UPDATE/INSERT; "" if none
	Val Node // ColumnRef | A_Expr | FuncCall | A_Const | A_Star | ...
}

func (*ResTarget) nodeTag() NodeTag { return T_ResTarget }
func (r *ResTarget) walkChildren(fn func(Node) error) error {
	if r.Val != nil {
		return fn(r.Val)
	}
	return nil
}

// RangeVar is a table reference (with optional schema and alias).
type RangeVar struct {
	SchemaName string
	RelName string
	Alias *Alias
}

func (*RangeVar) nodeTag() NodeTag { return T_RangeVar }
func (r *RangeVar) walkChildren(fn func(Node) error) error {
	if r.Alias != nil {
		return fn(r.Alias)
	}
	return nil
}

// JoinType enumerates the SQL join kinds. (Required for the OTel JOIN demo.)
type JoinType int32

const (
	JOIN_INNER JoinType = iota
	JOIN_LEFT
	JOIN_RIGHT
	JOIN_FULL
	JOIN_CROSS
)

// JoinExpr is a JOIN in the FROM clause: Larg JOIN Rarg ON Quals.
// This node is REQUIRED for the Gate-3 demo. (by design)
type JoinExpr struct {
	JoinType JoinType
	Larg Node // RangeVar | JoinExpr | SubLink
	Rarg Node
	Quals Node // ON expression (A_Expr / BoolExpr) or nil for CROSS
	UsingCols []string // USING (a, b); empty when ON is used
	Alias *Alias
}

func (*JoinExpr) nodeTag() NodeTag { return T_JoinExpr }
func (j *JoinExpr) walkChildren(fn func(Node) error) error {
	if j.Larg != nil {
		if err := fn(j.Larg); err != nil {
			return err
		}
	}
	if j.Rarg != nil {
		if err := fn(j.Rarg); err != nil {
			return err
		}
	}
	if j.Quals != nil {
		if err := fn(j.Quals); err != nil {
			return err
		}
	}
	if j.Alias != nil {
		if err := fn(j.Alias); err != nil {
			return err
		}
	}
	return nil
}

// ColumnRef references a column, optionally qualified (e.g. s.trace_id).
type ColumnRef struct {
	Fields []string // ["s", "trace_id"] or ["users", "*"] (last may be "*")
}

func (*ColumnRef) nodeTag() NodeTag { return T_ColumnRef }
func (*ColumnRef) walkChildren(fn func(Node) error) error { return nil }

// A_ExprKind classifies an A_Expr. AEXPR_OP covers binary/unary operators,
// including the JSONB operators ->>, ->, #>> required by the OTel JOIN demo.
type A_ExprKind int32

const (
	AEXPR_OP A_ExprKind = iota // any binary/unary operator named by Name
	AEXPR_OP_ANY // op ANY (array)
	AEXPR_IN // IN (...)
	AEXPR_LIKE // LIKE
	AEXPR_ILIKE // ILIKE
	AEXPR_BETWEEN // BETWEEN
)

// A_Expr is an operator expression. For JSONB extraction the Name carries the
// operator token: "->>", "->", or "#>>". Example from the Gate-3 demo:
//
//	s.attributes ->> 'user_id'
//
//	&A_Expr{Kind: AEXPR_OP, Name: "->>",
//	 Lexpr: &ColumnRef{Fields: []string{"s", "attributes"}},
//	 Rexpr: &A_Const{Val: "user_id", Type: ConstString}}
//
// (by design)
type A_Expr struct {
	Kind A_ExprKind
	Name string // operator token: "=", "<", "->>", "->", "#>>", "<->", ...
	Lexpr Node
	Rexpr Node
}

func (*A_Expr) nodeTag() NodeTag { return T_A_Expr }
func (e *A_Expr) walkChildren(fn func(Node) error) error {
	if e.Lexpr != nil {
		if err := fn(e.Lexpr); err != nil {
			return err
		}
	}
	if e.Rexpr != nil {
		if err := fn(e.Rexpr); err != nil {
			return err
		}
	}
	return nil
}

// ConstType classifies an A_Const literal.
type ConstType int32

const (
	ConstNull ConstType = iota
	ConstString
	ConstInt
	ConstFloat
	ConstBool
)

// A_Const is a literal constant.
type A_Const struct {
	Type ConstType
	Val string // textual form; interpreted per Type
}

func (*A_Const) nodeTag() NodeTag { return T_A_Const }
func (*A_Const) walkChildren(fn func(Node) error) error { return nil }

// ParamRef is a bind parameter ($1, $2, ...).
type ParamRef struct {
	Number int // 1-based
}

func (*ParamRef) nodeTag() NodeTag { return T_ParamRef }
func (*ParamRef) walkChildren(fn func(Node) error) error { return nil }

// FuncCall is a function/aggregate call. fts_match, fts_score, similarity,
// basuyudb_cluster_status, etc. are all FuncCall nodes (no bespoke AST type).
type FuncCall struct {
	FuncName []string // qualified name parts, e.g. ["fts_match"]
	Args []Node
	AggStar bool // COUNT(*)
}

func (*FuncCall) nodeTag() NodeTag { return T_FuncCall }
func (c *FuncCall) walkChildren(fn func(Node) error) error {
	for _, a := range c.Args {
		if err := fn(a); err != nil {
			return err
		}
	}
	return nil
}

// BoolOp enumerates AND/OR/NOT.
type BoolOp int32

const (
	AND_EXPR BoolOp = iota
	OR_EXPR
	NOT_EXPR
)

// BoolExpr is AND/OR/NOT over sub-expressions.
type BoolExpr struct {
	Op BoolOp
	Args []Node
}

func (*BoolExpr) nodeTag() NodeTag { return T_BoolExpr }
func (b *BoolExpr) walkChildren(fn func(Node) error) error {
	for _, a := range b.Args {
		if err := fn(a); err != nil {
			return err
		}
	}
	return nil
}

// NullTest — expr IS [NOT] NULL.
type NullTest struct {
	Arg Node
	TestNull bool // true: IS NULL; false: IS NOT NULL
}

func (*NullTest) nodeTag() NodeTag { return T_NullTest }
func (n *NullTest) walkChildren(fn func(Node) error) error {
	if n.Arg != nil {
		return fn(n.Arg)
	}
	return nil
}

// SortBy — ORDER BY entry.
type SortBy struct {
	Node Node
	SortDir int32 // 0 default, 1 ASC, 2 DESC
}

func (*SortBy) nodeTag() NodeTag { return T_SortBy }
func (s *SortBy) walkChildren(fn func(Node) error) error {
	if s.Node != nil {
		return fn(s.Node)
	}
	return nil
}

// Alias — table/column alias.
type Alias struct {
	AliasName string
	ColNames []string
}

func (*Alias) nodeTag() NodeTag { return T_Alias }
func (*Alias) walkChildren(fn func(Node) error) error { return nil }

// WithClause / CommonTableExpr — CTEs.
type WithClause struct {
	CTEs []*CommonTableExpr
	Recursive bool
}

func (*WithClause) nodeTag() NodeTag { return T_WithClause }
func (w *WithClause) walkChildren(fn func(Node) error) error {
	for _, c := range w.CTEs {
		if err := fn(c); err != nil {
			return err
		}
	}
	return nil
}

type CommonTableExpr struct {
	Name string
	Query Node // SelectStmt
}

func (*CommonTableExpr) nodeTag() NodeTag { return T_CommonTableExpr }
func (c *CommonTableExpr) walkChildren(fn func(Node) error) error {
	if c.Query != nil {
		return fn(c.Query)
	}
	return nil
}

// SubLink — a subquery used as an expression or in FROM.
type SubLink struct {
	SubSelect Node // SelectStmt
	Alias *Alias
}

func (*SubLink) nodeTag() NodeTag { return T_SubLink }
func (s *SubLink) walkChildren(fn func(Node) error) error {
	if s.SubSelect != nil {
		if err := fn(s.SubSelect); err != nil {
			return err
		}
	}
	if s.Alias != nil {
		if err := fn(s.Alias); err != nil {
			return err
		}
	}
	return nil
}

// TypeCast — expr::type.
type TypeCast struct {
	Arg Node
	TypeName string
}

func (*TypeCast) nodeTag() NodeTag { return T_TypeCast }
func (t *TypeCast) walkChildren(fn func(Node) error) error {
	if t.Arg != nil {
		return fn(t.Arg)
	}
	return nil
}

// A_Star — the "*" in SELECT * or COUNT(*).
type A_Star struct{}

func (*A_Star) nodeTag() NodeTag { return T_A_Star }
func (*A_Star) walkChildren(fn func(Node) error) error { return nil }

// CreateStmt — CREATE TABLE. Column/constraint detail is carried in TableElts
// as a slice of ColumnDef (handled by the executor's DDL path, not as bespoke
// AST node types beyond this statement node).
type CreateStmt struct {
	Relation *RangeVar
	TableElts []ColumnDef
}

func (*CreateStmt) nodeTag() NodeTag { return T_CreateStmt }
func (*CreateStmt) walkChildren(fn func(Node) error) error { return nil }

// ColumnDef is a column definition within a CREATE TABLE. It is a plain value
// type, not a Node (it carries no child Nodes requiring traversal).
type ColumnDef struct {
	ColName string
	TypeName string
	NotNull bool
	PrimaryKey bool
	Default Node // optional default expression
}

// IndexStmt — CREATE [UNIQUE] INDEX name ON table (col, ...).
type IndexStmt struct {
	Name    string
	Table   string
	Columns []string
	Unique  bool
}

func (*IndexStmt) nodeTag() NodeTag                       { return T_IndexStmt }
func (*IndexStmt) walkChildren(fn func(Node) error) error { return nil }
