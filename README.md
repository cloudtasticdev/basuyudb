<h1 align="center">BasuyuDB</h1>

<p align="center">
  <b>One binary. PostgreSQL wire. Branch your schema per PR.<br/>
  SQL JOIN your observability data. ANN vector + BM25 full-text search in the same query.<br/>
  Three continents, one transaction boundary.</b>
</p>

---

BasuyuDB is a distributed, multi-model database engine. It stores data natively
in BadgerDB (pure Go, embedded LSM-tree), speaks the PostgreSQL wire protocol,
replicates over Raft, and unifies relational, observability, vector, and
full-text data behind one SQL surface — replacing PostgreSQL + Elasticsearch +
Pinecone + ClickHouse with a single, zero-CGo, distroless binary.

> Name origin: **Ba**dgerDB (storage) · **Su**rrealDB (multi-model) · **Yu**gabyteDB (distributed SQL).

## The query no other database runs in one statement against one engine

```sql
-- Correlate an error span to the user who triggered it and what they were charged.
SELECT s.trace_id, s.duration_ms, s.attributes ->> 'amount' AS amount, u.email
FROM   otel_spans s
JOIN   users u ON u.id = s.attributes ->> 'user_id'
WHERE  s.status = 'ERROR';
```

## Capabilities

| Capability | How |
|---|---|
| **PostgreSQL wire v3** | Connect with `psql`, Prisma, Drizzle, pgx, psycopg2 — standard connection string |
| **Relational SQL** | `CREATE TABLE`, `INSERT`, `SELECT … WHERE`, `UPDATE`, `DELETE`, JOINs |
| **Branch-per-PR** | `CREATE BRANCH feature FROM main` (O(1)) · copy-on-write reads · `MERGE BRANCH … INTO main` |
| **OTel JOIN** | OTLP gRPC ingestion → `otel_spans`, JOIN-able with relational data via JSONB `->>` |
| **Distributed consensus** | 3-node Raft (dragonboat) — forms, replicates, survives leader failure |
| **Snapshot isolation** | Percolator-style transactions over a managed-mode hybrid-logical-clock timestamp model |
| **Vector search** | HNSW — ANN with L2 / cosine / inner-product metrics |
| **Full-text search** | BM25 — ranked search, score predicates |

## Quick start

```bash
# Build (pure Go, zero CGo)
cd engine && CGO_ENABLED=0 go build -o basuyudb ./cmd/basuyudb

# Run locally (dev-mode trust auth)
BASUYUDB_DEV_MODE=true BASUYUDB_DATA_DIR=/tmp/basuyudb ./basuyudb

# Connect with any PostgreSQL client
psql -h localhost -p 5432 -U dev -d myapp -c "SELECT version();"
```

Build details and architecture: [`engine/`](engine/).
Kubernetes (K3s) deployment: [`deploy/helm/basuyudb`](deploy/helm/basuyudb).

## Build & test

```bash
cd engine
CGO_ENABLED=0 go build ./...          # build / lint (zero CGo)
CGO_ENABLED=1 go test -race ./...     # race detector requires CGo
CGO_ENABLED=0 go test ./...           # functional tests
```

## Deploy (K3s)

```bash
helm install basuyudb deploy/helm/basuyudb --set replicaCount=3
```

3-node StatefulSet on local NVMe (`local-path`), PodDisruptionBudget
(`minAvailable: 2`), required anti-affinity, init-container lock cleanup, 90s
graceful termination.

## Licence

Apache-2.0. No BSL / AGPL / SSPL dependencies.
