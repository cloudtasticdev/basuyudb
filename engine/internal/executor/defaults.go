package executor

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// serialBaseOID reports whether a type name is a SERIAL pseudo-type and, if so,
// the underlying integer OID. SERIAL columns become integer columns with an
// implicit auto-increment sequence default (matching PostgreSQL).
func serialBaseOID(typeName string) (uint32, bool) {
	switch strings.ToLower(strings.TrimSpace(typeName)) {
	case "serial", "serial4":
		return OIDInt4, true
	case "bigserial", "serial8":
		return OIDInt8, true
	case "smallserial", "serial2":
		return OIDInt2, true
	}
	return 0, false
}

// classifyDefault reduces a parsed DEFAULT expression to a small evaluable form
// so INSERT can materialize a value without re-parsing. Supports literals,
// now()/CURRENT_TIMESTAMP, and gen_random_uuid()/uuid_generate_v4().
func classifyDefault(n ast.Node) (*defaultSpec, error) {
	switch e := n.(type) {
	case *ast.A_Const:
		v := evalConst(e)
		if v.null {
			return &defaultSpec{Kind: "const", OID: OIDUnknown}, nil // DEFAULT NULL
		}
		return &defaultSpec{Kind: "const", Text: v.text, OID: v.oid}, nil

	case *ast.TypeCast:
		// e.g. DEFAULT 'active'::status — keep the inner literal, adopt the OID.
		spec, err := classifyDefault(e.Arg)
		if err != nil {
			return nil, err
		}
		if spec.Kind == "const" {
			spec.OID = oidForTypeName(e.TypeName)
		}
		return spec, nil

	case *ast.FuncCall:
		switch niladicName(strings.Join(e.FuncName, ".")) {
		case "now":
			return &defaultSpec{Kind: "now"}, nil
		case "uuid":
			return &defaultSpec{Kind: "uuid"}, nil
		}

	case *ast.ColumnRef:
		// Bare niladic SQL keywords parse as identifiers (CURRENT_TIMESTAMP).
		switch niladicName(strings.Join(e.Fields, ".")) {
		case "now":
			return &defaultSpec{Kind: "now"}, nil
		case "uuid":
			return &defaultSpec{Kind: "uuid"}, nil
		}
	}
	return nil, newExecError("0A000", "unsupported DEFAULT expression %T", n)
}

// niladicName canonicalizes the recognized zero-argument default functions.
func niladicName(name string) string {
	switch strings.ToLower(name) {
	case "now", "current_timestamp", "transaction_timestamp", "statement_timestamp", "clock_timestamp":
		return "now"
	case "gen_random_uuid", "uuid_generate_v4":
		return "uuid"
	}
	return strings.ToLower(name)
}

// evalDefault materializes a column default into a concrete Datum. now()/uuid
// are resolved here (once, on the executing node) so the replicated mutation
// carries the same bytes to every replica.
func (e *execImpl) evalDefault(ctx context.Context, txn *transactions.Txn, sess *session.Session, spec *defaultSpec) (Datum, error) {
	switch spec.Kind {
	case "const":
		if spec.OID == OIDUnknown && spec.Text == "" {
			return Datum{Null: true}, nil
		}
		return Datum{Text: spec.Text}, nil
	case "now":
		return Datum{Text: time.Now().UTC().Format("2006-01-02 15:04:05.999999")}, nil
	case "uuid":
		u, err := randomUUIDv4()
		if err != nil {
			return Datum{}, err
		}
		return Datum{Text: u}, nil
	case "serial":
		n, err := e.nextSequenceVal(ctx, txn, sess, spec.Seq)
		if err != nil {
			return Datum{}, err
		}
		return Datum{Text: strconv.FormatInt(n, 10)}, nil
	default:
		return Datum{}, newExecError("XX000", "unknown default kind %q", spec.Kind)
	}
}

// seqKey is the namespace-scoped counter key for a named sequence. Stored beside
// schema metadata (like index lists) under a "#seq#" discriminator.
func (e *execImpl) seqKey(sess *session.Session, name string) storage.Key {
	return e.store.Encoder().SchemaKey(sess.Namespace(), "#seq#"+name)
}

// nextSequenceVal atomically returns the next value of a sequence within the
// current transaction. Concurrent inserts conflict on the counter key and one
// retries — yielding gap-free, collision-free auto-increment.
func (e *execImpl) nextSequenceVal(ctx context.Context, txn *transactions.Txn, sess *session.Session, name string) (int64, error) {
	key := e.seqKey(sess, name)
	cur := int64(0)
	raw, err := e.txn.Get(ctx, txn, key)
	switch {
	case err == nil:
		cur, _ = strconv.ParseInt(string(raw), 10, 64)
	case errors.Is(err, storage.ErrKeyNotFound):
		cur = 0
	default:
		return 0, err
	}
	next := cur + 1
	e.txn.Buffer(txn, transactions.Mutation{Key: key, Value: []byte(strconv.FormatInt(next, 10))})
	return next, nil
}

// applyGeneratedColumns recomputes every GENERATED ALWAYS AS (expr) STORED
// column from the other cells of the row. Any user-supplied value is overwritten
// (PostgreSQL forbids supplying one; recomputing is the safe, lossless choice).
// Called on INSERT and UPDATE after defaults are materialized. The generation
// expression may reference sibling columns but not other generated columns'
// freshly computed values in a defined order — we evaluate left-to-right so a
// later generated column can read an earlier one's new value.
func (e *execImpl) applyGeneratedColumns(sch *tableSchema, table string, cells []Datum, params []Datum) error {
	for i, c := range sch.Cols {
		if !c.Generated || c.GeneratedExpr == "" {
			continue
		}
		node, err := parseStoredExpr(c.GeneratedExpr)
		if err != nil {
			return err
		}
		ev := &evaluator{params: params, resolveCol: rowResolver(sch, table, cells)}
		v, err := ev.eval(node)
		if err != nil {
			return err
		}
		cells[i] = Datum{Null: v.null, Text: v.text}
	}
	return nil
}

// randomUUIDv4 returns a random RFC-4122 v4 UUID string.
func randomUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", newExecError("XX000", "uuid generation failed: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
