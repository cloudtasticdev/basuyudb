package executor

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
)

// listTables enumerates the user table schemas in the session namespace by
// scanning the schema-metadata prefix (skipping the "#idx" index-definition
// entries). Used to synthesize catalog views for ORM introspection.
func (e *execImpl) listTables(ctx context.Context, txn *transactions.Txn, sess *session.Session) ([]*tableSchema, error) {
	prefix := e.store.Encoder().SchemaKey(sess.Namespace(), "")
	it := e.txn.NewIterator(txn, prefix)
	defer it.Close()
	plen := len(prefix.Bytes())
	var out []*tableSchema
	for it.Rewind(); it.Valid(); it.Next() {
		tail := string(it.Key().Bytes()[plen:])
		if strings.Contains(tail, "#idx") || strings.Contains(tail, "#seq") || strings.Contains(tail, "#view") {
			continue // index/sequence/view metadata entry, not a table schema
		}
		raw, err := it.Value()
		if err != nil {
			return nil, err
		}
		var sch tableSchema
		if err := json.Unmarshal(raw, &sch); err != nil {
			continue
		}
		out = append(out, &sch)
	}
	return out, nil
}

// sqlTypeName maps a PG type OID to its information_schema data_type name.
func sqlTypeName(oid uint32) string {
	switch oid {
	case OIDInt2:
		return "smallint"
	case OIDInt4:
		return "integer"
	case OIDInt8:
		return "bigint"
	case OIDFloat4:
		return "real"
	case OIDFloat8:
		return "double precision"
	case OIDNumeric:
		return "numeric"
	case OIDBool:
		return "boolean"
	case OIDUUID:
		return "uuid"
	case OIDJSON:
		return "json"
	case OIDJSONB:
		return "jsonb"
	case OIDBytea:
		return "bytea"
	case OIDDate:
		return "date"
	case OIDTime:
		return "time without time zone"
	case OIDTimestamp:
		return "timestamp without time zone"
	case OIDTimestamptz:
		return "timestamp with time zone"
	case OIDVarchar:
		return "character varying"
	case OIDBpchar:
		return "character"
	default:
		return "text"
	}
}

// udtName maps a PG type OID to its internal udt_name (pg_type name).
func udtName(oid uint32) string {
	switch oid {
	case OIDInt2:
		return "int2"
	case OIDInt4:
		return "int4"
	case OIDInt8:
		return "int8"
	case OIDFloat4:
		return "float4"
	case OIDFloat8:
		return "float8"
	case OIDNumeric:
		return "numeric"
	case OIDBool:
		return "bool"
	case OIDUUID:
		return "uuid"
	case OIDJSON:
		return "json"
	case OIDJSONB:
		return "jsonb"
	case OIDBytea:
		return "bytea"
	case OIDDate:
		return "date"
	case OIDTime:
		return "time"
	case OIDTimestamp:
		return "timestamp"
	case OIDTimestamptz:
		return "timestamptz"
	case OIDVarchar:
		return "varchar"
	case OIDBpchar:
		return "bpchar"
	default:
		return "text"
	}
}

func textCol(name string) colMeta { return colMeta{Name: name, TypeOID: OIDText} }
func intCol(name string) colMeta  { return colMeta{Name: name, TypeOID: OIDInt4} }

// catalogVirtualTable returns the schema and rows of a supported catalog view
// (information_schema.* / pg_catalog.*), synthesized from the user tables, or
// ok=false if the (schema, table) pair is not a catalog view we model.
func (e *execImpl) catalogVirtualTable(ctx context.Context, txn *transactions.Txn, sess *session.Session, schemaName, tableName string) (*tableSchema, [][]Datum, bool, error) {
	sn := strings.ToLower(schemaName)
	tn := strings.ToLower(tableName)
	if sn != "information_schema" && sn != "pg_catalog" {
		return nil, nil, false, nil
	}
	tables, err := e.listTables(ctx, txn, sess)
	if err != nil {
		return nil, nil, false, err
	}

	switch {
	case sn == "information_schema" && tn == "tables":
		sch := &tableSchema{Name: "tables", PKIndex: -1, Cols: []colMeta{
			textCol("table_catalog"), textCol("table_schema"), textCol("table_name"), textCol("table_type"),
		}}
		rows := make([][]Datum, 0, len(tables))
		for _, t := range tables {
			rows = append(rows, []Datum{
				{Text: "defaultdb"}, {Text: "public"}, {Text: t.Name}, {Text: "BASE TABLE"},
			})
		}
		return sch, rows, true, nil

	case sn == "information_schema" && tn == "columns":
		sch := &tableSchema{Name: "columns", PKIndex: -1, Cols: []colMeta{
			textCol("table_catalog"), textCol("table_schema"), textCol("table_name"),
			textCol("column_name"), intCol("ordinal_position"), textCol("is_nullable"),
			textCol("data_type"), textCol("udt_name"),
		}}
		var rows [][]Datum
		for _, t := range tables {
			for i, c := range t.Cols {
				nullable := "YES"
				if c.NotNull {
					nullable = "NO"
				}
				rows = append(rows, []Datum{
					{Text: "defaultdb"}, {Text: "public"}, {Text: t.Name},
					{Text: c.Name}, {Text: itoaInt(i + 1)}, {Text: nullable},
					{Text: sqlTypeName(c.TypeOID)}, {Text: udtName(c.TypeOID)},
				})
			}
		}
		return sch, rows, true, nil

	case sn == "pg_catalog" && tn == "pg_tables":
		sch := &tableSchema{Name: "pg_tables", PKIndex: -1, Cols: []colMeta{
			textCol("schemaname"), textCol("tablename"), textCol("tableowner"),
		}}
		rows := make([][]Datum, 0, len(tables))
		for _, t := range tables {
			rows = append(rows, []Datum{{Text: "public"}, {Text: t.Name}, {Text: "basuyudb"}})
		}
		return sch, rows, true, nil
	}
	return nil, nil, false, nil
}

func itoaInt(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
