package hnsw

import (
	"math"
	"testing"
)

// TestUpsertAndANN proves vectors can be upserted and ANN search returns the
// nearest neighbour first (Gate 6 core: upsert + ANN query).
func TestUpsertAndANN(t *testing.T) {
	idx := New(4, L2)
	vecs := map[string][]float32{
		"a": {1, 0, 0, 0},
		"b": {0, 1, 0, 0},
		"c": {0, 0, 1, 0},
		"d": {0.9, 0.1, 0, 0}, // closest to query below
	}
	for id, v := range vecs {
		if err := idx.Upsert(id, v); err != nil {
			t.Fatal(err)
		}
	}
	if idx.Len() != 4 {
		t.Fatalf("want 4 vectors, got %d", idx.Len())
	}

	res, err := idx.Search([]float32{1, 0, 0, 0}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 || res[0].ID != "a" {
		t.Fatalf("nearest to (1,0,0,0) should be 'a', got %#v", res)
	}
}

// TestSnapshotRoundTrip proves the index survives a snapshot save+restore
// (Gate 6: index survives node restart without full rebuild).
func TestSnapshotRoundTrip(t *testing.T) {
	idx := New(3, Cosine)
	_ = idx.Upsert("x", []float32{1, 2, 3})
	_ = idx.Upsert("y", []float32{3, 2, 1})

	blob, err := idx.Snapshot()
	if err != nil {
		t.Fatal(err)
	}

	restored := New(3, Cosine)
	if err := restored.LoadSnapshot(blob); err != nil {
		t.Fatal(err)
	}
	if restored.Len() != 2 {
		t.Fatalf("restored index want 2 vectors, got %d", restored.Len())
	}
	res, _ := restored.Search([]float32{1, 2, 3}, 1)
	if len(res) != 1 || res[0].ID != "x" {
		t.Fatalf("restored search want 'x', got %#v", res)
	}
}

// TestCosineRecall checks recall@1 on a small random-ish set for cosine metric.
func TestCosineRecall(t *testing.T) {
	idx := New(8, Cosine)
	// Deterministic vectors (no rand: derived from index).
	base := func(seed int) []float32 {
		v := make([]float32, 8)
		for i := range v {
			v[i] = float32(math.Sin(float64(seed*13+i*7))) // deterministic spread
		}
		return v
	}
	const n = 200
	for i := 0; i < n; i++ {
		if err := idx.Upsert(itoa(i), base(i)); err != nil {
			t.Fatal(err)
		}
	}
	// Query equals vector 137 → recall@1 must return 137.
	res, _ := idx.Search(base(137), 1)
	if len(res) != 1 || res[0].ID != itoa(137) {
		t.Fatalf("recall@1 failed: want 137, got %#v", res)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
