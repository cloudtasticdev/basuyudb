package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/cloudtasticdev/basuyudb/engine/internal/version"
)

// startTime records when the process started; used to compute uptime.
var startTime = time.Now()

// healthResponse is the JSON body returned by /health and /healthz.
type healthResponse struct {
	Status string `json:"status"`
	Version string `json:"version"`
	Engine string `json:"engine"`
	Timestamp string `json:"timestamp"`
	UptimeSeconds int64 `json:"uptime_seconds"`
}

// PGPinger is the minimal interface required for the engine health check.
// pgxpool.Pool satisfies this interface.
type PGPinger interface {
	Ping(ctx context.Context) error
}

// NewMux returns an http.ServeMux wired with /health, /healthz, and /metrics.
//
// - logger: structured logger; may be nil.
// - pgPool: used for the /health engine reachability check; may be nil.
// - jwksCacheAge: returns the JWKS cache age in seconds; may be nil.
func NewMux(logger *slog.Logger, pgPool PGPinger, jwksCacheAge func() float64) *http.ServeMux {
	mux := http.NewServeMux()

	healthHandler := buildHealthHandler(pgPool)
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/metrics", buildMetricsHandler(jwksCacheAge))

	if os.Getenv("BASUYUDB_ENABLE_PPROF") == "true" {
		mux.HandleFunc("/debug/pprof/", http.DefaultServeMux.ServeHTTP)
		if logger != nil {
			logger.Warn("pprof endpoint enabled at /debug/pprof/")
		}
	}

	return mux
}

func buildHealthHandler(pgPool PGPinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		uptime := int64(time.Since(startTime).Seconds())
		now := time.Now().UTC()

		engineStatus := "connected"
		httpStatus := http.StatusOK

		if pgPool == nil {
			engineStatus = "unknown"
		} else if err := pgPool.Ping(r.Context()); err != nil {
			engineStatus = "unavailable"
			httpStatus = http.StatusServiceUnavailable
		}

		status := "ok"
		if httpStatus != http.StatusOK {
			status = "degraded"
		}

		resp := healthResponse{
			Status: status,
			Version: version.Number,
			Engine: engineStatus,
			Timestamp: now.Format(time.RFC3339),
			UptimeSeconds: uptime,
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatus)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func buildMetricsHandler(jwksCacheAge func() float64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		c := Global

		// ── Connection metrics ─────────────────────────────────────────────────
		fmt.Fprintf(w, "# HELP basuyudb_connections_active Current active PG wire connections\n")
		fmt.Fprintf(w, "# TYPE basuyudb_connections_active gauge\n")
		fmt.Fprintf(w, "basuyudb_connections_active %d\n\n", c.ConnectionsActive.Load())

		fmt.Fprintf(w, "# HELP basuyudb_connections_total Total PG wire connections accepted\n")
		fmt.Fprintf(w, "# TYPE basuyudb_connections_total counter\n")
		fmt.Fprintf(w, "basuyudb_connections_total %d\n\n", c.ConnectionsTotal.Load())

		// ── Query metrics ──────────────────────────────────────────────────────
		fmt.Fprintf(w, "# HELP basuyudb_queries_total Total queries executed\n")
		fmt.Fprintf(w, "# TYPE basuyudb_queries_total counter\n")
		fmt.Fprintf(w, "basuyudb_queries_total{result=\"ok\"} %d\n", c.QueriesOK.Load())
		fmt.Fprintf(w, "basuyudb_queries_total{result=\"error\"} %d\n\n", c.QueriesError.Load())

		// ── Query duration histogram ───────────────────────────────────────────
		durationSumSecs := float64(c.QueryDurationSum.Load()) / 1e9
		fmt.Fprintf(w, "# HELP basuyudb_query_duration_seconds Query execution duration histogram\n")
		fmt.Fprintf(w, "# TYPE basuyudb_query_duration_seconds histogram\n")
		fmt.Fprintf(w, "basuyudb_query_duration_seconds_bucket{le=\"0.001\"} %d\n", c.QueryDurationBucket1ms.Load())
		fmt.Fprintf(w, "basuyudb_query_duration_seconds_bucket{le=\"0.01\"} %d\n", c.QueryDurationBucket10ms.Load())
		fmt.Fprintf(w, "basuyudb_query_duration_seconds_bucket{le=\"0.1\"} %d\n", c.QueryDurationBucket100ms.Load())
		fmt.Fprintf(w, "basuyudb_query_duration_seconds_bucket{le=\"1.0\"} %d\n", c.QueryDurationBucket1s.Load())
		fmt.Fprintf(w, "basuyudb_query_duration_seconds_bucket{le=\"+Inf\"} %d\n", c.QueryDurationBucketInf.Load())
		fmt.Fprintf(w, "basuyudb_query_duration_seconds_sum %.9f\n", durationSumSecs)
		fmt.Fprintf(w, "basuyudb_query_duration_seconds_count %d\n\n", c.QueryDurationCount.Load())

		// ── Branch metrics ─────────────────────────────────────────────────────
		fmt.Fprintf(w, "# HELP basuyudb_branches_total Total branch create/delete operations\n")
		fmt.Fprintf(w, "# TYPE basuyudb_branches_total counter\n")
		fmt.Fprintf(w, "basuyudb_branches_total{op=\"create\"} %d\n", c.BranchCreates.Load())
		fmt.Fprintf(w, "basuyudb_branches_total{op=\"delete\"} %d\n\n", c.BranchDeletes.Load())

		// ── OTLP metrics ───────────────────────────────────────────────────────
		fmt.Fprintf(w, "# HELP basuyudb_otlp_spans_ingested_total Total OTLP spans ingested\n")
		fmt.Fprintf(w, "# TYPE basuyudb_otlp_spans_ingested_total counter\n")
		fmt.Fprintf(w, "basuyudb_otlp_spans_ingested_total %d\n\n", c.SpansIngested.Load())

		// ── Per-SQLSTATE query errors ──────────────────────────────────────────
		fmt.Fprintf(w, "# HELP basuyudb_query_errors_total Total query errors by SQLSTATE\n")
		fmt.Fprintf(w, "# TYPE basuyudb_query_errors_total counter\n")
		hasAny := false
		c.QueryErrors.Range(func(k, v interface{}) bool {
			sqlstate := k.(string)
			count := v.(*atomic.Int64).Load()
			fmt.Fprintf(w, "basuyudb_query_errors_total{sqlstate=%q} %d\n", sqlstate, count)
			hasAny = true
			return true
		})
		if !hasAny {
			// Emit a zero baseline so Prometheus sees the metric on first scrape.
			fmt.Fprintf(w, "basuyudb_query_errors_total{sqlstate=\"00000\"} 0\n")
		}
		fmt.Fprintf(w, "\n")

		// ── JWKS cache age ─────────────────────────────────────────────────────
		if jwksCacheAge != nil {
			fmt.Fprintf(w, "# HELP basuyudb_jwks_cache_age_seconds Age of JWKS key cache in seconds\n")
			fmt.Fprintf(w, "# TYPE basuyudb_jwks_cache_age_seconds gauge\n")
			fmt.Fprintf(w, "basuyudb_jwks_cache_age_seconds %.1f\n", jwksCacheAge())
		}
	}
}
