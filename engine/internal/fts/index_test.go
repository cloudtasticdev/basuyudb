package fts

import "testing"

// TestIndexAndRank proves documents can be indexed and full-text search returns
// relevance-ranked results, with the most relevant document first (Gate 8 core:
// fts_match returns ranked results).
func TestIndexAndRank(t *testing.T) {
	idx, err := NewMemory("standard")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	docs := map[string]string{
		"d1": "connection refused while dialing the auth service",
		"d2": "the user logged in successfully",
		"d3": "connection refused: connection refused on retry to auth",
		"d4": "disk full error writing value log",
	}
	for id, text := range docs {
		if err := idx.Index(id, text); err != nil {
			t.Fatal(err)
		}
	}

	hits, err := idx.Search("connection refused", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("want >=2 hits for 'connection refused', got %d", len(hits))
	}
	// d3 mentions the phrase twice → should outrank d1.
	if hits[0].DocID != "d3" {
		t.Fatalf("most relevant should be d3, got %q (score %v)", hits[0].DocID, hits[0].Score)
	}
	// Scores must be descending.
	for i := 1; i < len(hits); i++ {
		if hits[i].Score > hits[i-1].Score {
			t.Fatalf("scores not descending at %d", i)
		}
	}
	// d2/d4 (no match) must not appear.
	for _, h := range hits {
		if h.DocID == "d2" || h.DocID == "d4" {
			t.Fatalf("non-matching doc %q in results", h.DocID)
		}
	}
}

// TestScoreSingleDoc proves the fts_match/fts_score predicate path: a specific
// doc matches a query (score > 0) or does not.
func TestScoreSingleDoc(t *testing.T) {
	idx, _ := NewMemory("standard")
	defer idx.Close()
	_ = idx.Index("a", "vector search with approximate nearest neighbours")
	_ = idx.Index("b", "relational rows and transactions")

	score, ok, err := idx.Score("a", "nearest neighbours")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || score <= 0 {
		t.Fatalf("doc a should match 'nearest neighbours' with score>0, got ok=%v score=%v", ok, score)
	}
	_, ok, _ = idx.Score("b", "nearest neighbours")
	if ok {
		t.Fatal("doc b should not match 'nearest neighbours'")
	}
}

// TestDeleteRemoves proves deletion drops a doc from results.
func TestDeleteRemoves(t *testing.T) {
	idx, _ := NewMemory("standard")
	defer idx.Close()
	_ = idx.Index("x", "ephemeral document about badgers")
	if hits, _ := idx.Search("badgers", 5); len(hits) != 1 {
		t.Fatalf("want 1 hit before delete, got %d", len(hits))
	}
	_ = idx.Delete("x")
	if hits, _ := idx.Search("badgers", 5); len(hits) != 0 {
		t.Fatalf("want 0 hits after delete, got %d", len(hits))
	}
}
