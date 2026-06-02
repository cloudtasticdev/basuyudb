// Package hnsw provides BasuyuDB's approximate-nearest-neighbour vector index
// (ADR-010). It wraps coder/hnsw (CC0). The graph is serialised to a snapshot
// blob (Snapshot) which the caller persists to the managed BadgerDB store under
// a VectorKey within a transaction, so vector indexes survive node restarts
// without a full rebuild (Gate 6). Vector indexes are branch-local (Decision
// ).
//
// Distance metrics map to the SQL operators: <-> (L2/Euclidean), <=> (cosine),
// <#> (negative inner product).
package hnsw

import (
	"bytes"
	"fmt"
	"sync"

	chnsw "github.com/coder/hnsw"
)

// Metric selects the distance function.
type Metric int

const (
	L2 Metric = iota // <-> Euclidean
	Cosine // <=> cosine distance
	InnerProduct // <#> (negative) inner product
)

// MetricFromOperator maps a SQL vector operator token to a Metric.
func MetricFromOperator(op string) (Metric, bool) {
	switch op {
	case "<->":
		return L2, true
	case "<=>":
		return Cosine, true
	case "<#>":
		return InnerProduct, true
	}
	return 0, false
}

func distanceFunc(m Metric) chnsw.DistanceFunc {
	switch m {
	case Cosine:
		return chnsw.CosineDistance
	case InnerProduct:
		return func(a, b []float32) float32 {
			var dot float32
			for i := range a {
				dot += a[i] * b[i]
			}
			return -dot
		}
	default:
		return chnsw.EuclideanDistance
	}
}

// Result is one ANN hit.
type Result struct {
	ID string
	Distance float32
}

// Index is an in-memory HNSW vector index with snapshot serialisation. One
// Index serves a single (table, column) on one branch.
type Index struct {
	mu sync.Mutex
	graph *chnsw.Graph[string]
	dims int
	metric Metric
}

// New creates an empty vector index for vectors of the given dimensionality.
func New(dims int, metric Metric) *Index {
	g := chnsw.NewGraph[string]()
	g.Distance = distanceFunc(metric)
	g.M = 16
	g.Ml = 0.25
	g.EfSearch = 100
	return &Index{graph: g, dims: dims, metric: metric}
}

// Upsert adds or replaces a vector by id.
func (i *Index) Upsert(id string, vec []float32) error {
	if len(vec) != i.dims {
		return fmt.Errorf("hnsw: vector for %q has %d dims, index expects %d", id, len(vec), i.dims)
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.graph.Add(chnsw.MakeNode(id, vec))
	return nil
}

// UpsertBatch adds many vectors at once.
func (i *Index) UpsertBatch(ids []string, vecs [][]float32) error {
	if len(ids) != len(vecs) {
		return fmt.Errorf("hnsw: ids/vecs length mismatch")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	nodes := make([]chnsw.Node[string], 0, len(ids))
	for k := range ids {
		if len(vecs[k]) != i.dims {
			return fmt.Errorf("hnsw: vector %q has %d dims, want %d", ids[k], len(vecs[k]), i.dims)
		}
		nodes = append(nodes, chnsw.MakeNode(ids[k], vecs[k]))
	}
	i.graph.Add(nodes...)
	return nil
}

// Search returns the k approximate nearest neighbours of query, ordered by
// ascending distance.
func (i *Index) Search(query []float32, k int) ([]Result, error) {
	if len(query) != i.dims {
		return nil, fmt.Errorf("hnsw: query has %d dims, index expects %d", len(query), i.dims)
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	hits := i.graph.Search(query, k)
	out := make([]Result, 0, len(hits))
	for _, h := range hits {
		out = append(out, Result{ID: h.Key, Distance: i.graph.Distance(query, h.Value)})
	}
	return out, nil
}

// Len returns the number of indexed vectors.
func (i *Index) Len() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.graph.Len()
}

// Snapshot serialises the graph to a blob for persistence under a VectorKey.
func (i *Index) Snapshot() ([]byte, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	var buf bytes.Buffer
	if err := i.graph.Export(&buf); err != nil {
		return nil, fmt.Errorf("hnsw: export snapshot: %w", err)
	}
	return buf.Bytes(), nil
}

// LoadSnapshot restores the graph from a snapshot blob.
func (i *Index) LoadSnapshot(b []byte) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(b) == 0 {
		return nil
	}
	if err := i.graph.Import(bytes.NewReader(b)); err != nil {
		return fmt.Errorf("hnsw: import snapshot: %w", err)
	}
	i.graph.Distance = distanceFunc(i.metric) // Import may reset the func
	return nil
}
