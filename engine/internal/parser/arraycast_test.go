package parser

import (
	"testing"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
)

// TestArrayLiteralCastParses covers the `'literal'::TYPE[]` and `expr::TYPE[]`
// cast forms. Before the fix, cast_type did not accept the `[]` array suffix, so
// `'{1,2,3}'::int[]` raised a parse error even though `ARRAY[1,2,3]` parsed fine.
func TestArrayLiteralCastParses(t *testing.T) {
	cases := []struct {
		sql      string
		wantType string
	}{
		{"SELECT '{1,2,3}'::int[]", "int[]"},
		{"SELECT '{a,b}'::text[]", "text[]"},
		{"SELECT ARRAY[1,2]::int[]", "int[]"},
	}
	for _, c := range cases {
		n, err := Parse(c.sql)
		if err != nil {
			t.Fatalf("parse %q: %v", c.sql, err)
		}
		sel := n.(*ast.SelectStmt)
		tc, ok := sel.TargetList[0].Val.(*ast.TypeCast)
		if !ok {
			t.Fatalf("%q: want *ast.TypeCast target, got %T", c.sql, sel.TargetList[0].Val)
		}
		if tc.TypeName != c.wantType {
			t.Fatalf("%q: want cast type %q, got %q", c.sql, c.wantType, tc.TypeName)
		}
	}

	// The array-overlap operator over two literal-cast arrays must also parse
	// (this is the exact client/ORM query from the bug report).
	if _, err := Parse("SELECT '{1,2,3}'::int[] && '{3,4}'::int[]"); err != nil {
		t.Fatalf("parse overlap-of-casts: %v", err)
	}
}
