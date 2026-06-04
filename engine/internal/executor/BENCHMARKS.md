# BasuyuDB engine — performance baseline

Repeatable microbenchmarks live in `bench_micro_test.go` (Go `testing.B`, ns/op +
allocs/op — reliable below the Windows wall-clock resolution that made the older
`TestScaleBenchmark` wall-time probe noisy). Run:

```
go test ./internal/executor -run '^$' -bench 'Benchmark' -benchtime 1s
```

The scale probe (100k rows, end-to-end latency percentiles) is still available:

```
BASUYUDB_BENCH=1 BASUYUDB_BENCH_DIR=<roomy-dir> go test ./internal/executor -run TestScaleBenchmark -v -timeout 20m
```

## Scale probe (100k-row table, single node)

| Path | Result |
|---|---|
| INSERT (per-row, own txn + flush) | ~37.5k rows/sec, p99 1 ms |
| INSERT (50k rows, ONE txn) | ~342k rows/sec |
| Full table scan (100k rows) | 93 ms (~1.1M rows/sec) |
| PK point-get `WHERE id = X` | p99 ~536 µs |
| Indexed range `amount in [x,x+50]` | p99 ~997 µs |
| `ORDER BY amount DESC LIMIT 10` (index) | p99 sub-ms |

## Microbenchmarks (ns/op, allocs/op)

| Benchmark | Before | After parser pool |
|---|---|---|
| ParseSelect | 6465 ns, 21192 B, 21 allocs | **3766 ns, 712 B, 20 allocs** |
| PointLookupPK (parse+exec) | 14618 ns, 27184 B, 107 allocs | **13970 ns, 6708 B, 106 allocs** |
| PointLookupPK (exec only) | 10614 ns, 5984 B, 86 allocs | — |
| IndexedRange (~50 of 100k) | 215931 ns, 70594 B, 1397 allocs | — |
| SeqScan1k | 1237213 ns, 889332 B, 11278 allocs | — |
| InsertOne (autocommit) | 42290 ns, 30070 B, 159 allocs | — |
| RLS off / on (point-get) | 11808 ns / 17216 ns | — |

## Optimization landed

**Parser instance pooling** (`parser.Parse`): the goyacc `yyParse()` allocated a
fresh `*yyParserImpl` — which embeds the parse value-stack inline
(`[16]yySymType`) — on every call, the single largest allocation of a typical
parse, paid by every query (simple-query AND extended-protocol Parse). Pooling
the instance via `sync.Pool` reuses the stack: **−97% bytes / −42% time** on
ParseSelect, **−75% bytes** on the full parse+exec point-get. GC pressure per
query collapses, compounding under concurrent load. Safe because the generated
`Parse` re-initializes all state on entry; guarded by `TestParserPoolConcurrent`.

## Documented findings (future optimization targets, not regressions)

- **RLS preserves the index fast-path.** `execSelectFrom` runs `planIndexScan`
  first and `applyRLSSelect` filters the already-narrowed result — enabling RLS
  does NOT turn a point-get into a full scan. The ~5 µs/row RLS cost is inherent
  per-row policy-predicate evaluation (PostgreSQL evaluates RLS per row too). The
  allocations come from a per-row resolver closure + evaluator in
  `applyRLSSelect`/`rlsRowAllowed`; hoisting the policy-predicate setup out of the
  per-row loop would reduce them on large RLS-table scans.
- **Seq scan ~11 allocs/row.** Row decode + per-cell `Datum` construction +
  projection allocate per row; a row-buffer reuse / fewer per-cell allocations
  would help scans and ranges.
- **Single-row autocommit INSERT (~42 µs)** is dominated by the per-statement
  txn + flush; apps that batch (multi-row VALUES, COPY) already hit the ~342k
  rows/sec batch path.
