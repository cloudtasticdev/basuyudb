package ast

import "testing"

// TestOTelJoinShape proves the canonical AST can express the Gate-3 OTel JOIN
// demo query, including a JoinExpr and the JSONB ->> operator (the integration review):
//
//	SELECT s.trace_id, u.email
//	FROM otel_spans s JOIN users u ON u.id = s.attributes->>'user_id'
//	WHERE s.status = 'ERROR'
func TestOTelJoinShape(t *testing.T) {
	stmt := &SelectStmt{
		TargetList: []*ResTarget{
			{Val: &ColumnRef{Fields: []string{"s", "trace_id"}}},
			{Val: &ColumnRef{Fields: []string{"u", "email"}}},
		},
		FromClause: []Node{
			&JoinExpr{
				JoinType: JOIN_INNER,
				Larg: &RangeVar{RelName: "otel_spans", Alias: &Alias{AliasName: "s"}},
				Rarg: &RangeVar{RelName: "users", Alias: &Alias{AliasName: "u"}},
				Quals: &A_Expr{
					Kind: AEXPR_OP,
					Name: "=",
					Lexpr: &ColumnRef{Fields: []string{"u", "id"}},
					Rexpr: &A_Expr{
						Kind: AEXPR_OP,
						Name: "->>", // JSONB text extraction
						Lexpr: &ColumnRef{Fields: []string{"s", "attributes"}},
						Rexpr: &A_Const{Type: ConstString, Val: "user_id"},
					},
				},
			},
		},
		WhereClause: &A_Expr{
			Kind: AEXPR_OP,
			Name: "=",
			Lexpr: &ColumnRef{Fields: []string{"s", "status"}},
			Rexpr: &A_Const{Type: ConstString, Val: "ERROR"},
		},
	}

	if TagOf(stmt) != T_SelectStmt {
		t.Fatalf("unexpected tag %d", TagOf(stmt))
	}

	// Walk must reach the JoinExpr and the ->> operator.
	var sawJoin, sawJSONB bool
	err := Walk(stmt, func(n Node) error {
		switch v := n.(type) {
		case *JoinExpr:
			sawJoin = true
		case *A_Expr:
			if v.Name == "->>" {
				sawJSONB = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if !sawJoin {
		t.Fatal("Walk did not reach the JoinExpr node")
	}
	if !sawJSONB {
		t.Fatal("Walk did not reach the JSONB ->> operator")
	}
}

// TestWalkAbort proves Walk propagates an error from the visitor and stops.
func TestWalkAbort(t *testing.T) {
	stmt := &SelectStmt{
		TargetList: []*ResTarget{{Val: &A_Star{}}},
		FromClause: []Node{&RangeVar{RelName: "t"}},
	}
	sentinel := errStop
	got := Walk(stmt, func(n Node) error {
		if TagOf(n) == T_RangeVar {
			return sentinel
		}
		return nil
	})
	if got != sentinel {
		t.Fatalf("expected sentinel error from Walk, got %v", got)
	}
}

var errStop = &walkErr{"stop"}

type walkErr struct{ s string }

func (e *walkErr) Error() string { return e.s }
