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
	T_DropStmt
	T_TruncateStmt
	T_AlterTableStmt
	T_List
	T_CaseExpr
	T_CaseWhen
	T_CreateViewStmt
	T_CopyStmt
	T_SetToDefault
	T_ShowStmt
	T_SavepointStmt
	T_CreateSeqStmt
	T_CreateSchemaStmt
	T_CreateExtensionStmt
	T_CreateEnumStmt
	T_ListenStmt
	T_DoNothingStmt
	T_ExplainStmt
	T_RowExpr
	T_FieldSelect
	T_CreatePolicyStmt
	T_AlterPolicyStmt
	T_DropPolicyStmt
	T_CreateTypeStmt
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
	Distinct bool // SELECT DISTINCT
	// DistinctOn carries the expression list of SELECT DISTINCT ON (exprs).
	// When set, Distinct is also true. Empty/nil for plain DISTINCT or none.
	DistinctOn []Node
	TargetList []*ResTarget
	FromClause []Node // RangeVar | JoinExpr | SubLink (subquery in FROM)
	WhereClause Node // boolean expression or nil
	GroupClause []Node
	HavingClause Node
	SortClause []*SortBy
	LimitCount Node // A_Const | ParamRef | nil
	LimitOffset Node

	// Set operation. When SetOp != SetOpNone, this node is a set-operation node:
	// Larg/Rarg are the operands and the body fields above are empty except the
	// trailing SortClause/LimitCount/LimitOffset, which apply to the whole result.
	SetOp SetOpType
	All bool // UNION ALL / INTERSECT ALL / EXCEPT ALL
	Larg *SelectStmt
	Rarg *SelectStmt
}

// SetOpType enumerates the SQL set operations.
type SetOpType int32

const (
	SetOpNone SetOpType = iota
	SetOpUnion
	SetOpIntersect
	SetOpExcept
)

func (*SelectStmt) nodeTag() NodeTag { return T_SelectStmt }
func (s *SelectStmt) walkChildren(fn func(Node) error) error {
	if s.Larg != nil {
		if err := fn(s.Larg); err != nil {
			return err
		}
	}
	if s.Rarg != nil {
		if err := fn(s.Rarg); err != nil {
			return err
		}
	}
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
	MultiRows  [][]Node // non-nil when multi-row VALUES; each inner slice is one row
	ReturningList []*ResTarget
	OnConflict *OnConflictClause // optional ON CONFLICT action
}

// OnConflictClause models INSERT ... ON CONFLICT [(cols)] DO NOTHING | DO UPDATE
// SET .... DoUpdateSet assignments may reference EXCLUDED.<col> (the proposed
// insert row).
type OnConflictClause struct {
	Columns []string // conflict target columns (informational; PK is matched)
	DoNothing bool
	DoUpdateSet []*ResTarget
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
	Relation      *RangeVar
	UsingClause   []Node // DELETE ... USING table_list
	WhereClause   Node
	ReturningList []*ResTarget
}

func (*DeleteStmt) nodeTag() NodeTag { return T_DeleteStmt }
func (s *DeleteStmt) walkChildren(fn func(Node) error) error {
	if s.Relation != nil {
		if err := fn(s.Relation); err != nil {
			return err
		}
	}
	for _, u := range s.UsingClause {
		if err := fn(u); err != nil {
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
	AEXPR_OP_ANY               // op ANY (array)
	AEXPR_IN                   // IN (...)
	AEXPR_LIKE                 // LIKE
	AEXPR_ILIKE                // ILIKE
	AEXPR_BETWEEN              // BETWEEN
	AEXPR_NOT_LIKE             // NOT LIKE
	AEXPR_NOT_ILIKE            // NOT ILIKE
	AEXPR_SIMILAR_TO           // SIMILAR TO
	AEXPR_NOT_SIMILAR_TO       // NOT SIMILAR TO
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

// RowExpr is a ROW(...) constructor or the bare implicit row form
// `(a, b, ...)` used in e.g. `WHERE (a,b) IN (...)`. Items holds the row's
// element expressions in order.
type RowExpr struct {
	Items []Node
}

func (*RowExpr) nodeTag() NodeTag { return T_RowExpr }
func (r *RowExpr) walkChildren(fn func(Node) error) error {
	for _, it := range r.Items {
		if it == nil {
			continue
		}
		if err := fn(it); err != nil {
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
// When used as a FROM-clause table function (e.g. generate_series), Alias
// carries the AS alias and optional column aliases.
type FuncCall struct {
	FuncName    []string   // qualified name parts, e.g. ["fts_match"]
	Args        []Node
	AggStar     bool       // COUNT(*)
	AggDistinct bool       // COUNT(DISTINCT x)
	Over        *WindowDef // non-nil when called as a window function: f(...) OVER (...)
	Filter      Node       // FILTER (WHERE expr) for filtered aggregates
	WithinGroup *WindowDef // WITHIN GROUP (ORDER BY ...) for ordered-set aggregates
	Alias       *Alias     // non-nil when used as a set-returning function in FROM
}

// WindowDef is the OVER (...) specification of a window function call.
type WindowDef struct {
	PartitionBy []Node
	OrderBy []*SortBy
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
	Name  string
	Cols  []string // optional column list for RECURSIVE CTEs: WITH name(cols) AS (...)
	Query Node     // SelectStmt
}

func (*CommonTableExpr) nodeTag() NodeTag { return T_CommonTableExpr }
func (c *CommonTableExpr) walkChildren(fn func(Node) error) error {
	if c.Query != nil {
		return fn(c.Query)
	}
	return nil
}

// List — a parenthesised list of expressions, e.g. the right side of
// `x IN (1, 2, 3)`.
type List struct {
	Items []Node
}

func (*List) nodeTag() NodeTag { return T_List }
func (l *List) walkChildren(fn func(Node) error) error {
	for _, it := range l.Items {
		if err := fn(it); err != nil {
			return err
		}
	}
	return nil
}

// SubLink — a subquery used as an expression or in FROM.
type SubLink struct {
	SubSelect Node // SelectStmt
	Alias *Alias
	Exists bool // EXISTS (subquery)
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

// RangeSubselect is a subquery in the FROM clause with an alias.
// When Lateral is true the subquery was preceded by the LATERAL keyword and
// may reference columns from preceding FROM items.
type RangeSubselect struct {
	Subquery Node   // SelectStmt
	Alias    *Alias // always non-nil; the alias is required
	Lateral  bool   // LATERAL keyword was present
}

func (*RangeSubselect) nodeTag() NodeTag { return T_SubLink } // reuse SubLink tag for now
func (r *RangeSubselect) walkChildren(fn func(Node) error) error {
	if r.Subquery != nil {
		if err := fn(r.Subquery); err != nil {
			return err
		}
	}
	if r.Alias != nil {
		if err := fn(r.Alias); err != nil {
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
	Relation    *RangeVar
	TableElts   []ColumnDef
	IfNotExists bool
	Temporary   bool        // CREATE TEMP TABLE / CREATE TEMPORARY TABLE
	AsSelect    *SelectStmt // non-nil for CREATE TABLE ... AS SELECT
	// TableConstraints carries table-level constraints declared inline in the
	// CREATE TABLE element list (composite PRIMARY KEY / UNIQUE / FOREIGN KEY /
	// CHECK). Each reuses AlterTableConstraint as its representation.
	TableConstraints []*AlterTableConstraint
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
	Unique bool  // inline UNIQUE column constraint
	Default Node // optional default expression
	Check Node   // optional CHECK (expr) constraint
	FKTable string  // REFERENCES target table (empty if no FK)
	FKColumn string // REFERENCES target column (empty -> parent PK)
	// FKName carries the optional `CONSTRAINT name` prefix of an inline
	// REFERENCES clause. FKDeferrable / FKInitiallyDeferred carry its
	// [NOT] DEFERRABLE and INITIALLY { DEFERRED | IMMEDIATE } flags. These are
	// folded from the column's ColQual list by NewColumnDef so the executor can
	// persist them for DEFERRABLE constraint checking (SET CONSTRAINTS).
	FKName              string
	FKDeferrable        bool
	FKInitiallyDeferred bool

	// Identity / IdentityAlways: GENERATED { ALWAYS | BY DEFAULT } AS IDENTITY.
	// The executor treats an identity column like SERIAL.
	Identity       bool
	IdentityAlways bool

	// Generated / GeneratedExpr: GENERATED ALWAYS AS ( expr ) STORED.
	Generated     bool
	GeneratedExpr Node

	// FKOnDelete / FKOnUpdate: referential actions for the inline REFERENCES
	// clause. Canonical strings ("CASCADE", "SET NULL", ...); empty if omitted.
	FKOnDelete string
	FKOnUpdate string
}

// CaseExpr is a CASE expression. Arg is non-nil for the simple form
// (CASE x WHEN v THEN ...), nil for the searched form (CASE WHEN cond THEN ...).
// Else is optional (NULL when absent).
type CaseExpr struct {
	Arg   Node
	Whens []*CaseWhen
	Else  Node
}

func (*CaseExpr) nodeTag() NodeTag { return T_CaseExpr }
func (c *CaseExpr) walkChildren(fn func(Node) error) error {
	if c.Arg != nil {
		if err := fn(c.Arg); err != nil {
			return err
		}
	}
	for _, w := range c.Whens {
		if err := fn(w); err != nil {
			return err
		}
	}
	if c.Else != nil {
		return fn(c.Else)
	}
	return nil
}

// CaseWhen is one WHEN arm of a CASE. For a searched CASE, Cond is a boolean
// predicate; for a simple CASE, Cond is the value compared against CaseExpr.Arg.
type CaseWhen struct {
	Cond   Node
	Result Node
}

func (*CaseWhen) nodeTag() NodeTag { return T_CaseWhen }
func (w *CaseWhen) walkChildren(fn func(Node) error) error {
	if err := fn(w.Cond); err != nil {
		return err
	}
	return fn(w.Result)
}

// ColQualKind enumerates the inline column constraints (NOT NULL, DEFAULT, ...)
// that may follow a column's type in any order in a CREATE TABLE.
type ColQualKind int

const (
	ColQualNotNull ColQualKind = iota
	ColQualNull
	ColQualPrimaryKey
	ColQualUnique
	ColQualDefault
	ColQualReferences // FOREIGN KEY: REFERENCES table[(col)]
	ColQualCheck      // CHECK (expr)
	ColQualIdentity   // GENERATED { ALWAYS | BY DEFAULT } AS IDENTITY
	ColQualGenerated  // GENERATED ALWAYS AS ( expr ) STORED
)

// ColQual is one inline column constraint. Expr is set for ColQualDefault and
// ColQualCheck; RefTable/RefCol for ColQualReferences.
type ColQual struct {
	Kind ColQualKind
	Expr Node
	RefTable string
	RefCol string
	// ConstraintName is the optional `CONSTRAINT name` prefix.
	ConstraintName string
	// Deferrable / InitiallyDeferred carry [NOT] DEFERRABLE and
	// INITIALLY { DEFERRED | IMMEDIATE }. Parsed and stored for
	// compatibility (Rails/Django migrations); not enforced single-node.
	Deferrable        bool
	InitiallyDeferred bool

	// Identity / IdentityAlways carry GENERATED { ALWAYS | BY DEFAULT } AS
	// IDENTITY (Kind == ColQualIdentity). IdentityAlways is true for ALWAYS,
	// false for BY DEFAULT. The executor treats identity like SERIAL.
	Identity       bool
	IdentityAlways bool

	// Generated / GeneratedExpr carry GENERATED ALWAYS AS ( expr ) STORED
	// (Kind == ColQualGenerated). GeneratedExpr is the generation expression.
	Generated     bool
	GeneratedExpr Node

	// OnDelete / OnUpdate carry the trailing referential actions of a
	// REFERENCES / FOREIGN KEY clause (Kind == ColQualReferences). Canonical
	// strings: "NO ACTION", "RESTRICT", "CASCADE", "SET NULL", "SET DEFAULT".
	// Empty when omitted.
	OnDelete string
	OnUpdate string
}

// NewColumnDef folds an ordered list of inline qualifiers onto a base column
// definition, so the grammar can accept constraints in any order.
func NewColumnDef(name, typeName string, quals []ColQual) ColumnDef {
	cd := ColumnDef{ColName: name, TypeName: typeName}
	for _, q := range quals {
		switch q.Kind {
		case ColQualNotNull:
			cd.NotNull = true
		case ColQualNull:
			cd.NotNull = false
		case ColQualPrimaryKey:
			cd.PrimaryKey = true
			cd.NotNull = true
		case ColQualUnique:
			cd.Unique = true
		case ColQualDefault:
			cd.Default = q.Expr
		case ColQualReferences:
			cd.FKTable = q.RefTable
			cd.FKColumn = q.RefCol
			cd.FKOnDelete = q.OnDelete
			cd.FKOnUpdate = q.OnUpdate
			cd.FKName = q.ConstraintName
			cd.FKDeferrable = q.Deferrable
			cd.FKInitiallyDeferred = q.InitiallyDeferred
		case ColQualCheck:
			cd.Check = q.Expr
		case ColQualIdentity:
			cd.Identity = true
			cd.IdentityAlways = q.IdentityAlways
			cd.NotNull = true
		case ColQualGenerated:
			cd.Generated = true
			cd.GeneratedExpr = q.GeneratedExpr
		}
	}
	return cd
}

// CreateViewStmt — CREATE [OR REPLACE] [MATERIALIZED] VIEW name AS query.
type CreateViewStmt struct {
	Relation     *RangeVar
	Query        Node   // SelectStmt
	Replace      bool
	Materialized bool   // CREATE MATERIALIZED VIEW
	IfNotExists  bool   // CREATE MATERIALIZED VIEW IF NOT EXISTS
}

func (*CreateViewStmt) nodeTag() NodeTag { return T_CreateViewStmt }
func (s *CreateViewStmt) walkChildren(fn func(Node) error) error {
	if s.Query != nil {
		return fn(s.Query)
	}
	return nil
}

// SetToDefault is the DEFAULT keyword used as a value in an INSERT VALUES list
// (ORMs like Drizzle emit it for serial/defaulted columns). It instructs the
// executor to apply the column's default rather than an explicit value.
type SetToDefault struct{}

func (*SetToDefault) nodeTag() NodeTag                       { return T_SetToDefault }
func (*SetToDefault) walkChildren(fn func(Node) error) error { return nil }

// ShowStmt — SHOW <name> (a run-time configuration parameter / GUC).
type ShowStmt struct {
	Name string
}

func (*ShowStmt) nodeTag() NodeTag                       { return T_ShowStmt }
func (*ShowStmt) walkChildren(fn func(Node) error) error { return nil }

// CopyStmt — COPY table [(cols)] FROM STDIN / TO STDOUT, or COPY (query) TO
// STDOUT. IsFrom distinguishes load (FROM STDIN) from export (TO STDOUT).
type CopyStmt struct {
	Table     string
	Columns   []string
	Query     Node // SelectStmt for COPY (query) TO STDOUT; nil otherwise
	IsFrom    bool
	Format    string // "text" (default) or "csv"
	Delimiter string // single-char delimiter; "" = format default
	Header    bool   // CSV HEADER
}

func (*CopyStmt) nodeTag() NodeTag { return T_CopyStmt }
func (s *CopyStmt) walkChildren(fn func(Node) error) error {
	if s.Query != nil {
		return fn(s.Query)
	}
	return nil
}

// IndexElem is one element of a CREATE INDEX column list. Exactly one of
// ColName (a plain column index element) or Expr (an expression index element,
// e.g. ( lower(name) )) is set.
type IndexElem struct {
	ColName string
	Expr    Node
}

// IndexStmt — CREATE [UNIQUE] INDEX name ON table (elem, ...) [WHERE pred].
// Columns retains the plain column-name list for backwards compatibility; it
// holds the ColName of each element and is empty in slots where the element is
// an expression. Elems carries the full per-element detail (column or
// expression). Where is the optional partial-index predicate.
type IndexStmt struct {
	Name    string
	Table   string
	Columns []string
	Elems   []IndexElem
	Where   Node
	Unique  bool
}

func (*IndexStmt) nodeTag() NodeTag                       { return T_IndexStmt }
func (*IndexStmt) walkChildren(fn func(Node) error) error { return nil }

// DropStmt — DROP TABLE [IF EXISTS] name, and other DROP statements.
type DropStmt struct {
	Table       string
	IfExists    bool
	IsView      bool // DROP VIEW vs DROP TABLE
	IsSequence  bool // DROP SEQUENCE
	IsSchema    bool // DROP SCHEMA
	IsExtension bool // DROP EXTENSION
	IsType      bool // DROP TYPE
}

func (*DropStmt) nodeTag() NodeTag                       { return T_DropStmt }
func (*DropStmt) walkChildren(fn func(Node) error) error { return nil }

// TruncateStmt — TRUNCATE [TABLE] name.
type TruncateStmt struct {
	Table string
}

func (*TruncateStmt) nodeTag() NodeTag                       { return T_TruncateStmt }
func (*TruncateStmt) walkChildren(fn func(Node) error) error { return nil }

// AlterTableKind classifies an ALTER TABLE action.
type AlterTableKind int32

const (
	AlterAddColumn AlterTableKind = iota
	AlterDropColumn
	AlterRenameTable
	AlterRenameColumn // RENAME COLUMN old TO new
	// ALTER [COLUMN] colname ... actions:
	AlterColumnType // ALTER COLUMN col TYPE typename [USING expr]
	AlterSetDefault // ALTER COLUMN col SET DEFAULT expr
	AlterDropDefault // ALTER COLUMN col DROP DEFAULT
	AlterSetNotNull // ALTER COLUMN col SET NOT NULL
	AlterDropNotNull // ALTER COLUMN col DROP NOT NULL
	// ADD [CONSTRAINT name] PRIMARY KEY | UNIQUE | FOREIGN KEY | CHECK:
	AlterAddConstraint
	// ALTER TABLE ... { ENABLE | DISABLE | FORCE | NO FORCE } ROW LEVEL SECURITY:
	AlterEnableRLS  // ENABLE ROW LEVEL SECURITY
	AlterDisableRLS // DISABLE ROW LEVEL SECURITY
	AlterForceRLS   // FORCE ROW LEVEL SECURITY
	AlterNoForceRLS // NO FORCE ROW LEVEL SECURITY
)

// AlterTableCmd is a single action within an ALTER TABLE statement.
// Subtype carries the action kind; Name is the existing object name (column,
// constraint, etc.) and NewName is the target name for renames.
type AlterTableCmd struct {
	Subtype AlterTableKind
	Name    string // existing column / constraint name
	NewName string // target name for renames
	// TypeName / UsingExpr carry ALTER COLUMN ... TYPE typename [USING expr].
	TypeName  string
	UsingExpr Node
	// DefExpr carries the SET DEFAULT expression (AlterSetDefault).
	DefExpr Node
	// Constraint carries the detail for AlterAddConstraint.
	Constraint *AlterTableConstraint
}

// ConstraintType classifies an ADD CONSTRAINT action.
type ConstraintType int32

const (
	ConstrPrimaryKey ConstraintType = iota
	ConstrUnique
	ConstrForeignKey
	ConstrCheck
)

// AlterTableConstraint carries the detail of an
// ALTER TABLE ... ADD [CONSTRAINT name] { PRIMARY KEY | UNIQUE | FOREIGN KEY |
// CHECK } clause.
type AlterTableConstraint struct {
	ConstraintType ConstraintType
	Name           string   // optional CONSTRAINT name
	Columns        []string // local columns (PK / UNIQUE / FK source columns)
	// FOREIGN KEY targets:
	RefTable   string
	RefColumns []string
	OnDelete   string // canonical referential action; "" if omitted
	OnUpdate   string
	// CHECK predicate:
	Expr Node
	// Deferrable / InitiallyDeferred carry [NOT] DEFERRABLE and
	// INITIALLY { DEFERRED | IMMEDIATE } for a table-level / ADD CONSTRAINT
	// constraint. The current grammar does not parse these for table-level
	// constraints, so they remain false there (table-level FKs are
	// non-deferrable); they are present so the executor reads deferrability
	// uniformly across constraint paths.
	Deferrable        bool
	InitiallyDeferred bool
}

// AlterTableStmt — ALTER TABLE name ADD COLUMN col type | DROP COLUMN col |
// RENAME TO name | RENAME COLUMN old TO new.
//
// The Cmds slice carries multi-action ALTER TABLE (used by the new grammar
// rules). Single-action rules continue to populate the flat fields for
// backwards compatibility with the executor.
type AlterTableStmt struct {
	Table   string
	Kind    AlterTableKind
	Column  ColumnDef      // for ADD: full def; for DROP: only ColName is set
	NewName string         // for RENAME TO (table)
	Cmds    []AlterTableCmd // populated by RENAME COLUMN / RENAME CONSTRAINT etc.
	// Relation carries a *RangeVar when the grammar uses qualified_name
	// (needed by the ALTER INDEX stub).
	Relation *RangeVar
}

func (*AlterTableStmt) nodeTag() NodeTag                       { return T_AlterTableStmt }
func (*AlterTableStmt) walkChildren(fn func(Node) error) error { return nil }

// SavepointStmt — SAVEPOINT name / RELEASE SAVEPOINT name / ROLLBACK TO name.
type SavepointStmt struct {
	Name     string
	Release  bool // RELEASE SAVEPOINT
	Rollback bool // ROLLBACK TO SAVEPOINT
}

func (*SavepointStmt) nodeTag() NodeTag                       { return T_SavepointStmt }
func (*SavepointStmt) walkChildren(fn func(Node) error) error { return nil }

// CreateSeqStmt — CREATE [TEMP] SEQUENCE name [options].
type CreateSeqStmt struct {
	Sequence    *RangeVar
	IfNotExists bool
}

func (*CreateSeqStmt) nodeTag() NodeTag                       { return T_CreateSeqStmt }
func (*CreateSeqStmt) walkChildren(fn func(Node) error) error { return nil }

// CreateSchemaStmt — CREATE SCHEMA name.
type CreateSchemaStmt struct {
	SchemaName  string
	IfNotExists bool
}

func (*CreateSchemaStmt) nodeTag() NodeTag                       { return T_CreateSchemaStmt }
func (*CreateSchemaStmt) walkChildren(fn func(Node) error) error { return nil }

// CreateExtensionStmt — CREATE EXTENSION name.
type CreateExtensionStmt struct {
	ExtName     string
	IfNotExists bool
}

func (*CreateExtensionStmt) nodeTag() NodeTag                       { return T_CreateExtensionStmt }
func (*CreateExtensionStmt) walkChildren(fn func(Node) error) error { return nil }

// CreateEnumStmt — CREATE TYPE name AS ENUM (...).
type CreateEnumStmt struct {
	TypeName *RangeVar
	Vals     []string
}

func (*CreateEnumStmt) nodeTag() NodeTag                       { return T_CreateEnumStmt }
func (*CreateEnumStmt) walkChildren(fn func(Node) error) error { return nil }

// ListenStmt — LISTEN/NOTIFY/UNLISTEN channel.
type ListenStmt struct {
	Channel    string
	IsNotify   bool
	IsUnlisten bool
	Payload    string
}

func (*ListenStmt) nodeTag() NodeTag                       { return T_ListenStmt }
func (*ListenStmt) walkChildren(fn func(Node) error) error { return nil }

// DoNothingStmt is emitted for statements that BasuyuDB intentionally accepts
// but does not execute (e.g. CREATE FUNCTION, CREATE TRIGGER, COMMENT ON …).
// The executor returns a success result with Command "OK".
type DoNothingStmt struct{}

func (*DoNothingStmt) nodeTag() NodeTag                       { return T_DoNothingStmt }
func (*DoNothingStmt) walkChildren(fn func(Node) error) error { return nil }

// ExplainStmt — EXPLAIN [ANALYZE] [VERBOSE] stmt.
type ExplainStmt struct {
	Query   Node
	Analyze bool
	Verbose bool
}

func (*ExplainStmt) nodeTag() NodeTag                       { return T_ExplainStmt }
func (*ExplainStmt) walkChildren(fn func(Node) error) error { return nil }

// FieldSelect is composite-type field access: ( expr ).field. Arg is the row /
// composite value expression; Field is the selected attribute name.
type FieldSelect struct {
	Arg   Node
	Field string
}

func (*FieldSelect) nodeTag() NodeTag { return T_FieldSelect }
func (f *FieldSelect) walkChildren(fn func(Node) error) error {
	if f.Arg != nil {
		return fn(f.Arg)
	}
	return nil
}

// CreatePolicyStmt — CREATE POLICY name ON table
//   [AS { PERMISSIVE | RESTRICTIVE }]
//   [FOR { ALL | SELECT | INSERT | UPDATE | DELETE }]
//   [TO role_list] [USING (expr)] [WITH CHECK (expr)].
type CreatePolicyStmt struct {
	PolicyName string
	Table      string
	Permissive bool     // default true; RESTRICTIVE => false
	Command    string   // "ALL" (default) / "SELECT" / "INSERT" / "UPDATE" / "DELETE"
	Roles      []string // TO role_list; empty => PUBLIC
	Using      Node     // USING (expr) predicate, or nil
	WithCheck  Node     // WITH CHECK (expr) predicate, or nil
}

func (*CreatePolicyStmt) nodeTag() NodeTag { return T_CreatePolicyStmt }
func (s *CreatePolicyStmt) walkChildren(fn func(Node) error) error {
	if s.Using != nil {
		if err := fn(s.Using); err != nil {
			return err
		}
	}
	if s.WithCheck != nil {
		return fn(s.WithCheck)
	}
	return nil
}

// AlterPolicyStmt — ALTER POLICY name ON table [TO role_list]
//   [USING (expr)] [WITH CHECK (expr)].
type AlterPolicyStmt struct {
	PolicyName string
	Table      string
	Roles      []string
	Using      Node
	WithCheck  Node
}

func (*AlterPolicyStmt) nodeTag() NodeTag { return T_AlterPolicyStmt }
func (s *AlterPolicyStmt) walkChildren(fn func(Node) error) error {
	if s.Using != nil {
		if err := fn(s.Using); err != nil {
			return err
		}
	}
	if s.WithCheck != nil {
		return fn(s.WithCheck)
	}
	return nil
}

// DropPolicyStmt — DROP POLICY [IF EXISTS] name ON table.
type DropPolicyStmt struct {
	PolicyName string
	Table      string
	IfExists   bool
}

func (*DropPolicyStmt) nodeTag() NodeTag                       { return T_DropPolicyStmt }
func (*DropPolicyStmt) walkChildren(fn func(Node) error) error { return nil }

// CompositeField is one field of a composite type body: name and SQL type name.
type CompositeField struct {
	Name     string
	TypeName string
}

// CreateTypeStmt — CREATE TYPE name AS ( field type, ... ) (composite type).
type CreateTypeStmt struct {
	Name   string
	Fields []CompositeField
}

func (*CreateTypeStmt) nodeTag() NodeTag                       { return T_CreateTypeStmt }
func (*CreateTypeStmt) walkChildren(fn func(Node) error) error { return nil }
