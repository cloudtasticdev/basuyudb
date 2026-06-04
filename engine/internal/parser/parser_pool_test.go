package parser

import (
	"fmt"
	"sync"
	"testing"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
)

// TestParserPoolConcurrent guards the pooled-parser optimization: many
// goroutines parsing distinct statements concurrently must each get a correct,
// independent AST (no cross-talk through the shared sync.Pool of parser stacks).
func TestParserPoolConcurrent(t *testing.T) {
	const goroutines = 32
	const iters = 200
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				// Each goroutine uses a distinct table/column so a leaked stack
				// value would surface as a wrong identifier.
				sql := fmt.Sprintf("SELECT c%d FROM t%d WHERE c%d = %d", g, g, g, i)
				stmt, err := Parse(sql)
				if err != nil {
					errCh <- fmt.Errorf("g%d i%d: %v", g, i, err)
					return
				}
				sel, ok := stmt.(*ast.SelectStmt)
				if !ok || len(sel.TargetList) != 1 {
					errCh <- fmt.Errorf("g%d i%d: unexpected AST %T", g, i, stmt)
					return
				}
				cr, ok := sel.TargetList[0].Val.(*ast.ColumnRef)
				if !ok || len(cr.Fields) == 0 || cr.Fields[len(cr.Fields)-1] != fmt.Sprintf("c%d", g) {
					errCh <- fmt.Errorf("g%d i%d: wrong target %#v", g, i, sel.TargetList[0].Val)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}
