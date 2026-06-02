# BasuyuDB

**One binary. PostgreSQL wire. Branch your schema per PR. SQL JOIN your observability data. ANN vector search and BM25 full-text search in the same query. Three continents, one transaction boundary. Apache-2.0.**

BasuyuDB is a distributed multi-model database engine. It stores data natively in
BadgerDB (pure Go, embedded LSM-tree), speaks the PostgreSQL wire protocol, runs
distributed consensus over Raft, and unifies relational, observability, vector,
and full-text data behind one SQL surface — replacing PostgreSQL + Elasticsearch
+ Pinecone + ClickHouse with a single, zero-CGo, distroless binary.

> Name origin: **Ba**dgerDB (storage) · **Su**rrealDB (multi-model) · **Yu**gabyteDB (distributed SQL).

## Why

Every application in 2026 runs at least three data stores — relational, vector,
and observability — that cannot share a transaction boundary, cannot be queried
together, and have no branch-per-PR schema workflow. BasuyuDB replaces all three.

The query no other database runs in one statement against one engine:

```sql
-- Correlate an error span to the user who triggered it and what they were charged.
SELECT s.trace_id, s.duration_ms, s.attributes ->> 'amount' AS amount, u.email
FROM   otel_spans s
JOIN   users u ON u.id = s.attributes ->> 'user_id'
WHERE  s.status = 'ERROR';
```

## Capabilities (V0.1)

| Capability | Status | How |
|---|---|---|
| PostgreSQL wire v3 | ✅ | Connect with `psql`, Prisma, Drizzle, pgx, psycopg2 — standard connection string |
| Relational SQL | ✅ | `CREATE TABLE`, `INSERT`, `SELECT … WHERE`, `UPDATE`, `DELETE`, JOINs |
| Branch-per-PR | ✅ | `CREATE BRANCH feature FROM main` (O(1)) · COW reads · `MERGE BRANCH … INTO main` (schema + data) |
| OTel JOIN | ✅ | OTLP gRPC ingestion → `otel_spans` table, JOIN-able with relational data via JSONB `->>` |
| Distributed consensus | ✅ | 3-node dragonboat Raft — forms, replicates, survives leader failure |
| Snapshot isolation | ✅ | Percolator-style transactions over a managed-mode HLC timestamp model |
| Vector search | ✅ | HNSW (coder/hnsw) — ANN with L2 / cosine / inner-product metrics |
| Full-text search | ✅ | BM25 (bleve) — ranked search, score predicates |
| Air-gapped licensing | designed | Offline CapabilityBundle, ML-DSA-65 signed (ADR-021) |

## Architecture

```
PG wire (5432) ─┐
OTLP gRPC (4317)─┤→ parser (goyacc, PG grammar) → executor ─┐
                │                                            ├→ transactions (Percolator + HLC)
                │                                            │       │ Commit → Propose
                │                                            │       ▼
                │                              consensus (dragonboat Raft) ─→ state machine
                │                                            │       │ applies batch
                └────────────────────────────────────────────┴───────▼
                                              storage (managed BadgerDB, KeyEncoder)
                                       branch COW · HNSW vectors · bleve FTS · OTel spans
```

Every component is built against a single frozen contract set
(`the design specs`): one `ast.Node`, one `storage.KeyEncoder`,
one `transactions.TransactionEngine`, one cycle-free `auth ← session ← executor
← wire` import layering.

## Build & run

```bash
# Build (pure Go, zero CGo — ADR-002)
CGO_ENABLED=0 go build -o basuyudb ./cmd/basuyudb

# Run locally (dev-mode trust auth)
BASUYUDB_DEV_MODE=true BASUYUDB_DATA_DIR=/tmp/basuyudb ./basuyudb

# Connect
psql -h localhost -p 5432 -U dev -d myapp -c "SELECT version();"
```

### Tests

```bash
CGO_ENABLED=0 go build ./...          # build / lint
CGO_ENABLED=1 go test -race ./...     # race detector requires CGo (Linux CI)
CGO_ENABLED=0 go test ./...           # functional tests
```

## Deploy (K3s)

```bash
helm install basuyudb deploy/helm/basuyudb --set replicaCount=3
```

The chart deploys a 3-node StatefulSet on local NVMe (`local-path`), with a
PodDisruptionBudget (`minAvailable: 2`), required anti-affinity, an init
container that clears stale BadgerDB/dragonboat locks, and a 90s graceful
termination window.

## Licence

Apache-2.0 (engine). The TypeScript SDK is ELv2. No BSL/AGPL/SSPL dependencies.
