// Package fts provides BasuyuDB's full-text search (ADR-017) using
// blevesearch/bleve (Apache 2.0, pure Go). Each index serves one (table, field)
// on one branch and is BM25-ranked. FTS indexes are branch-local (Decision
// ).
//
// V0.1 uses bleve's native scorch store persisted under the engine data
// directory. The canonical BadgerDB-KVStore adapter (the design specs), which
// unifies FTS storage into the single managed BadgerDB instance, is the
// documented follow-on; the SQL surface (fts_match / fts_score) and behaviour
// are identical either way.
package fts

import (
	"fmt"
	"sync"

	"github.com/blevesearch/bleve/v2"
)

// Hit is one ranked FTS result.
type Hit struct {
	DocID string
	Score float64
}

// Index is a BM25 full-text index over one logical (table, field).
type Index struct {
	mu sync.Mutex
	idx bleve.Index
}

// document is the indexed shape: a single analysed content field.
type document struct {
	Content string `json:"content"`
}

// NewMemory builds an in-memory FTS index (used in tests and ephemeral mode).
func NewMemory(analyzer string) (*Index, error) {
	return newIndex("", analyzer)
}

// NewPersistent builds (or opens) a persistent FTS index at path.
func NewPersistent(path, analyzer string) (*Index, error) {
	return newIndex(path, analyzer)
}

func newIndex(path, analyzer string) (*Index, error) {
	mapping := bleve.NewIndexMapping()
	if analyzer != "" {
		mapping.DefaultAnalyzer = analyzer
	}

	var (
		idx bleve.Index
		err error
	)
	if path == "" {
		idx, err = bleve.NewMemOnly(mapping)
	} else {
		idx, err = bleve.New(path, mapping)
		if err == bleve.ErrorIndexPathExists {
			idx, err = bleve.Open(path)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("fts: open index: %w", err)
	}
	return &Index{idx: idx}, nil
}

// Index adds or replaces a document's text under docID.
func (i *Index) Index(docID, text string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.idx.Index(docID, document{Content: text})
}

// Delete removes a document from the index.
func (i *Index) Delete(docID string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.idx.Delete(docID)
}

// Search runs a BM25-ranked match query and returns the top `limit` hits,
// highest score first.
func (i *Index) Search(query string, limit int) ([]Hit, error) {
	if limit <= 0 {
		limit = 10
	}
	i.mu.Lock()
	defer i.mu.Unlock()

	mq := bleve.NewMatchQuery(query)
	mq.SetField("content")
	req := bleve.NewSearchRequest(mq)
	req.Size = limit

	res, err := i.idx.Search(req)
	if err != nil {
		return nil, fmt.Errorf("fts: search: %w", err)
	}
	hits := make([]Hit, 0, len(res.Hits))
	for _, h := range res.Hits {
		hits = append(hits, Hit{DocID: h.ID, Score: h.Score})
	}
	return hits, nil
}

// Matches reports whether a document matches a query (used by fts_match in a
// WHERE predicate). It is a bounded search restricted to the candidate doc.
func (i *Index) Score(docID, query string) (float64, bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	mq := bleve.NewMatchQuery(query)
	mq.SetField("content")
	dq := bleve.NewDocIDQuery([]string{docID})
	conj := bleve.NewConjunctionQuery(mq, dq)
	req := bleve.NewSearchRequest(conj)
	req.Size = 1

	res, err := i.idx.Search(req)
	if err != nil {
		return 0, false, fmt.Errorf("fts: score: %w", err)
	}
	if len(res.Hits) == 0 {
		return 0, false, nil
	}
	return res.Hits[0].Score, true, nil
}

// Count returns the number of indexed documents.
func (i *Index) Count() (uint64, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.idx.DocCount()
}

// Close releases the index.
func (i *Index) Close() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.idx.Close()
}
