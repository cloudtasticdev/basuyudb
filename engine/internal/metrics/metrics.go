// Package metrics provides a lightweight atomic-counter metrics registry
// and an HTTP server exposing Prometheus text format and health endpoints.
// No external Prometheus client library is used — counters are hand-written
// in Prometheus exposition format (text/plain; version=0.0.4).
package metrics

import (
	"sync"
	"sync/atomic"
)

// Counters holds all BasuyuDB engine metrics as atomic integers.
// Access via the Global singleton; increment with the Add / Inc methods.
type Counters struct {
	// Connection metrics.
	ConnectionsActive atomic.Int64 // gauge: current active PG wire connections
	ConnectionsTotal atomic.Int64 // counter: total connections accepted

	// Query metrics.
	QueriesOK atomic.Int64 // counter: queries completed successfully
	QueriesError atomic.Int64 // counter: queries that returned an error

	// Query duration histogram buckets (cumulative counts, in seconds).
	// Buckets: 0.001, 0.01, 0.1, 1.0, +Inf
	QueryDurationBucket1ms atomic.Int64 // le=0.001
	QueryDurationBucket10ms atomic.Int64 // le=0.01
	QueryDurationBucket100ms atomic.Int64 // le=0.1
	QueryDurationBucket1s atomic.Int64 // le=1.0
	QueryDurationBucketInf atomic.Int64 // le=+Inf (== total observations)
	QueryDurationSum atomic.Int64 // sum * 1e9 (nanoseconds stored, converted to seconds on read)
	QueryDurationCount atomic.Int64 // total observations

	// Branch operation metrics.
	BranchCreates atomic.Int64 // counter: branch create operations
	BranchDeletes atomic.Int64 // counter: branch delete operations

	// OTLP metrics.
	SpansIngested atomic.Int64 // counter: total OTLP spans ingested

	// Per-SQLSTATE error counters — map[string]*atomic.Int64.
	QueryErrors sync.Map
}

// Global is the engine-wide metrics singleton.
// Other packages increment counters via this variable:
//
//	metrics.Global.ConnectionsActive.Add(1)
//	metrics.Global.RecordQueryError("42P01")
var Global = &Counters{}

// RecordQueryError increments the per-SQLSTATE query error counter.
// Creates a new counter for sqlstate on first use.
func (c *Counters) RecordQueryError(sqlstate string) {
	val, _ := c.QueryErrors.LoadOrStore(sqlstate, new(atomic.Int64))
	val.(*atomic.Int64).Add(1)
}

// RecordQueryDurationNs records a query duration (in nanoseconds) into the histogram.
func (c *Counters) RecordQueryDurationNs(ns int64) {
	secs := float64(ns) / 1e9
	if secs <= 0.001 {
		c.QueryDurationBucket1ms.Add(1)
	}
	if secs <= 0.01 {
		c.QueryDurationBucket10ms.Add(1)
	}
	if secs <= 0.1 {
		c.QueryDurationBucket100ms.Add(1)
	}
	if secs <= 1.0 {
		c.QueryDurationBucket1s.Add(1)
	}
	c.QueryDurationBucketInf.Add(1)
	c.QueryDurationSum.Add(ns)
	c.QueryDurationCount.Add(1)
}
