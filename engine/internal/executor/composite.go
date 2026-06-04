package executor

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// compositeType is a persisted CREATE TYPE name AS (fields) definition: the type
// name and its ordered fields. Stored as JSON under the "#composite#NAME"
// discriminator in the session namespace (mirrors how enums use "#enum#NAME").
type compositeType struct {
	Name   string              `json:"name"`
	Fields []compositeFieldDef `json:"fields"`
}

// compositeFieldDef is one field of a composite type: its name and type OID.
type compositeFieldDef struct {
	Name    string `json:"name"`
	TypeOID uint32 `json:"type_oid"`
}

// fieldIndex returns the position of a field by (case-insensitive) name, or -1.
func (ct *compositeType) fieldIndex(name string) int {
	for i, f := range ct.Fields {
		if strings.EqualFold(f.Name, name) {
			return i
		}
	}
	return -1
}

// compositeKey returns the namespace-scoped key for a stored composite type.
func (e *execImpl) compositeKey(sess *session.Session, name string) storage.Key {
	return e.store.Encoder().SchemaKey(sess.Namespace(), "#composite#"+name)
}

// execCreateType persists a CREATE TYPE name AS (fields) composite type. Each
// field's SQL type name is resolved to an OID via oidForTypeName (a composite or
// unknown type falls back to text).
func (e *execImpl) execCreateType(ctx context.Context, s *ast.CreateTypeStmt, sess *session.Session) (*Result, error) {
	txn, owns, err := e.beginTx(ctx, sess.Auth)
	if err != nil {
		return nil, err
	}
	defer e.rollbackTx(ctx, txn, owns)

	ct := compositeType{Name: s.Name, Fields: make([]compositeFieldDef, len(s.Fields))}
	for i, f := range s.Fields {
		ct.Fields[i] = compositeFieldDef{Name: f.Name, TypeOID: oidForTypeName(f.TypeName)}
	}
	raw, err := json.Marshal(&ct)
	if err != nil {
		return nil, newExecError("XX000", "encode composite type: %v", err)
	}
	e.txn.Buffer(txn, transactions.Mutation{Key: e.compositeKey(sess, s.Name), Value: raw})
	if err := e.commitTx(ctx, txn, owns); err != nil {
		return nil, err
	}
	return &Result{Command: "CREATE TYPE"}, nil
}

// loadComposite reads a composite type definition by name at the txn snapshot,
// returning (nil, false) when no such composite type exists.
func (e *execImpl) loadComposite(ctx context.Context, txn *transactions.Txn, sess *session.Session, name string) (*compositeType, bool) {
	raw, err := e.txn.Get(ctx, txn, e.compositeKey(sess, name))
	if err != nil {
		return nil, false
	}
	var ct compositeType
	if json.Unmarshal(raw, &ct) != nil {
		return nil, false
	}
	return &ct, true
}

// compositeResolverCtx builds a lookupComposite closure that resolves a composite
// type using the ambient transaction in ctx (or a short read txn when none is
// open). Used where the evaluator has no explicit txn handle (e.g. the FROM-less
// SELECT projection). Returns nil when sess is nil.
func (e *execImpl) compositeResolverCtx(ctx context.Context, sess *session.Session) func(string) (*compositeType, bool) {
	if sess == nil {
		return nil
	}
	return func(name string) (*compositeType, bool) {
		txn, owns, err := e.beginTx(ctx, sess.Auth)
		if err != nil {
			return nil, false
		}
		defer e.rollbackTx(ctx, txn, owns)
		return e.loadComposite(ctx, txn, sess, name)
	}
}

// decodeRecordText parses a PostgreSQL record literal "(a,b,c)" into its element
// text values, honoring double-quoted elements (with "" escapes) and treating an
// empty unquoted element as NULL. It is the inverse of encodeRowText.
func decodeRecordText(s string) ([]value, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '(' || s[len(s)-1] != ')' {
		return nil, false
	}
	body := s[1 : len(s)-1]
	var out []value
	if body == "" {
		return out, true
	}
	var b strings.Builder
	inQuote := false
	quoted := false // whether the current element was quoted (so "" != NULL)
	i := 0
	for i < len(body) {
		c := body[i]
		if inQuote {
			if c == '"' {
				if i+1 < len(body) && body[i+1] == '"' {
					b.WriteByte('"')
					i += 2
					continue
				}
				inQuote = false
				i++
				continue
			}
			b.WriteByte(c)
			i++
			continue
		}
		switch c {
		case '"':
			inQuote = true
			quoted = true
			i++
		case ',':
			out = append(out, recElem(b.String(), quoted))
			b.Reset()
			quoted = false
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	out = append(out, recElem(b.String(), quoted))
	return out, true
}

// recElem builds a record element value: an empty UNQUOTED element is NULL.
func recElem(text string, quoted bool) value {
	if text == "" && !quoted {
		return value{null: true}
	}
	return value{text: text, oid: OIDText}
}
