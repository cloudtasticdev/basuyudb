package executor

import (
	"context"

	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// scanRow is one materialised row with its storage key.
type scanRow struct {
	key storage.Key
	cells []Datum
}

// scanned is the result of scanning a single relation.
type scanned struct {
	schema *tableSchema
	alias string
	rows []scanRow
}

// OTelSpansTable is the built-in relation name for ingested OpenTelemetry spans.
const OTelSpansTable = "otel_spans"

// otelSpansSchema is the fixed schema of the otel_spans built-in table. Spans
// are written by OTLP ingestion under OtelSpanKey, which lands beneath the same
// data/otel_spans/ prefix that RowPrefix(otel_spans) scans — so the OTel table
// is scanned by the identical mechanism as any relational table. The
// `attributes` column holds a JSON document queryable with ->> (Gate 3).
var otelSpansSchema = &tableSchema{
	Name: OTelSpansTable,
	PKIndex: -1,
	Cols: []colMeta{
		{Name: "trace_id", TypeOID: OIDText},
		{Name: "span_id", TypeOID: OIDText},
		{Name: "parent_span_id", TypeOID: OIDText},
		{Name: "service_name", TypeOID: OIDText},
		{Name: "span_name", TypeOID: OIDText},
		{Name: "duration_ms", TypeOID: OIDInt8},
		{Name: "status", TypeOID: OIDText},
		{Name: "started_at", TypeOID: OIDText},
		{Name: "attributes", TypeOID: OIDText}, // JSON document
	},
}

// OTelSpanColumns returns the otel_spans column order (used by OTLP ingestion to
// encode span rows so the executor decodes them with the matching schema).
func OTelSpanColumns() []string {
	out := make([]string, len(otelSpansSchema.Cols))
	for i, c := range otelSpansSchema.Cols {
		out[i] = c.Name
	}
	return out
}

// resolveSchema returns the schema for a relation: the built-in otel_spans
// schema, or a catalog lookup for a user table.
func (e *execImpl) resolveSchema(ctx context.Context, txn *transactions.Txn, sess *session.Session, table string) (*tableSchema, error) {
	if table == OTelSpansTable {
		return otelSpansSchema, nil
	}
	return e.loadSchema(ctx, txn, sess, table)
}

// branchLocal reports whether a relation is branch-local (no COW fall-through
// to the parent). By design, FTS/vector/OTel indexes are branch-local;
// relational tables fall through. otel_spans is the built-in branch-local table.
func branchLocal(table string) bool { return table == OTelSpansTable }

// scanTable materialises every row of a relation on the session's branch. For a
// feature branch and a fall-through relation, rows present on the parent but not
// overridden on the branch are merged in (copy-on-write); a branch tombstone
// suppresses the parent row. (by design)
func (e *execImpl) scanTable(ctx context.Context, txn *transactions.Txn, sess *session.Session, table, alias string) (*scanned, error) {
	sch, err := e.resolveSchema(ctx, txn, sess, table)
	if err != nil {
		return nil, err
	}
	if alias == "" {
		alias = table
	}
	res := &scanned{schema: sch, alias: alias}
	enc := e.store.Encoder()
	branchName := sess.Branch()

	// Branch scan: collect rows keyed by their pk tail (key bytes after the
	// table's RowPrefix). Tombstones are recorded as suppressions.
	branchPrefix := enc.RowPrefix(sess.Namespace(), branchName, table)
	seen := map[string]bool{} // tails present on the branch (live or tombstone)
	bit := e.txn.NewIterator(txn, branchPrefix)
	for bit.Rewind(); bit.Valid(); bit.Next() {
		raw, err := bit.Value()
		if err != nil {
			bit.Close()
			return nil, err
		}
		tail := keyTail(bit.Key().Bytes(), branchPrefix.Bytes())
		seen[string(tail)] = true
		if storage.IsTombstone(raw) {
			continue // suppressed; do not emit
		}
		cells, err := decodeRow(raw, len(sch.Cols))
		if err != nil {
			bit.Close()
			return nil, err
		}
		res.rows = append(res.rows, scanRow{key: bit.Key(), cells: cells})
	}
	bit.Close()

	// Parent fall-through for relational tables on a feature branch.
	if branchName != "main" && !branchLocal(table) {
		parent := "main"
		if meta, err := e.branches.MetaFor(ctx, sess.Auth, branchName); err == nil && meta.Parent != "" {
			parent = meta.Parent
		}
		parentPrefix := enc.RowPrefix(sess.Namespace(), parent, table)
		pit := e.txn.NewIterator(txn, parentPrefix)
		for pit.Rewind(); pit.Valid(); pit.Next() {
			tail := keyTail(pit.Key().Bytes(), parentPrefix.Bytes())
			if seen[string(tail)] {
				continue // overridden or tombstoned on the branch
			}
			raw, err := pit.Value()
			if err != nil {
				pit.Close()
				return nil, err
			}
			if storage.IsTombstone(raw) {
				continue
			}
			cells, err := decodeRow(raw, len(sch.Cols))
			if err != nil {
				pit.Close()
				return nil, err
			}
			res.rows = append(res.rows, scanRow{key: pit.Key(), cells: cells})
		}
		pit.Close()
	}

	return res, nil
}

// keyTail returns the bytes of key after prefix (the row's pk-bearing tail,
// identical across branches for the same logical row).
func keyTail(key, prefix []byte) []byte {
	if len(key) < len(prefix) {
		return key
	}
	return key[len(prefix):]
}
