package executor

import (
	"context"
	"encoding/json"
	"strconv"
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
	case OIDChar:
		return "char"
	case OIDName:
		return "name"
	case OIDOid:
		return "oid"
	case OIDVoid:
		return "void"
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

	case sn == "pg_catalog" && tn == "pg_namespace":
		sch := &tableSchema{Name: "pg_namespace", PKIndex: -1, Cols: []colMeta{
			oidCol("oid"), textCol("nspname"), oidCol("nspowner"),
		}}
		rows := [][]Datum{
			{oidDatum(oidPublicSchema), {Text: "public"}, oidDatum(10)},
			{oidDatum(11), {Text: "pg_catalog"}, oidDatum(10)},
			{oidDatum(13000), {Text: "information_schema"}, oidDatum(10)},
		}
		return sch, rows, true, nil

	case sn == "pg_catalog" && tn == "pg_class":
		// Include all columns that Prisma/quaint introspection queries reference.
		sch := &tableSchema{Name: "pg_class", PKIndex: -1, Cols: []colMeta{
			pgOidCol("oid"), textCol("relname"), pgOidCol("relnamespace"),
			charCol("relkind"), pgOidCol("reltype"), boolCol("relhaspkey"),
			intCol("relnatts"), pgOidCol("relowner"),
			boolCol("relhassubclass"), boolCol("relispartition"),
			boolCol("relrowsecurity"), boolCol("relforcerowsecurity"), textCol("reloptions"),
		}}
		var rows [][]Datum
		for _, t := range tables {
			rows = append(rows, []Datum{
				oidDatum(tableOID(t.Name)), {Text: t.Name}, oidDatum(oidPublicSchema),
				{Text: "r"}, oidDatum(0), boolDatum(t.PKIndex >= 0),
				{Text: itoaInt(len(t.Cols))}, oidDatum(10),
				boolDatum(false), boolDatum(false), // relhassubclass=f, relispartition=f
				boolDatum(t.RLSEnabled), boolDatum(t.RLSForced), {Null: true}, // RLS flags, reloptions=NULL
			})
		}
		return sch, rows, true, nil

	case sn == "pg_catalog" && tn == "pg_policies":
		// pg_policies: one row per policy, the introspection view pg_dump and
		// admin tools read. qual/with_check are the deparsed predicate texts; roles
		// is a Postgres array literal ({public} when the policy applies to all).
		sch := &tableSchema{Name: "pg_policies", PKIndex: -1, Cols: []colMeta{
			textCol("schemaname"), textCol("tablename"), textCol("policyname"),
			textCol("permissive"), textCol("roles"), textCol("cmd"),
			textCol("qual"), textCol("with_check"),
		}}
		var rows [][]Datum
		for _, t := range tables {
			for i := range t.Policies {
				p := &t.Policies[i]
				rows = append(rows, []Datum{
					{Text: "public"}, {Text: t.Name}, {Text: p.Name},
					{Text: permissiveText(p.Permissive)}, {Text: rolesArrayLiteral(p.Roles)}, {Text: p.Command},
					optTextDatum(p.UsingExpr), optTextDatum(p.CheckExpr),
				})
			}
		}
		return sch, rows, true, nil

	case sn == "pg_catalog" && tn == "pg_attribute":
		sch := &tableSchema{Name: "pg_attribute", PKIndex: -1, Cols: []colMeta{
			oidCol("attrelid"), textCol("attname"), oidCol("atttypid"),
			intCol("attnum"), boolCol("attnotnull"), boolCol("attisdropped"),
			intCol("attlen"), intCol("atttypmod"),
		}}
		var rows [][]Datum
		for _, t := range tables {
			for i, c := range t.Cols {
				rows = append(rows, []Datum{
					oidDatum(tableOID(t.Name)), {Text: c.Name}, oidDatum(c.TypeOID),
					{Text: itoaInt(i + 1)}, boolDatum(c.NotNull), boolDatum(false),
					{Text: "-1"}, {Text: "-1"},
				})
			}
		}
		return sch, rows, true, nil

	case sn == "pg_catalog" && tn == "pg_type":
		// Columns must match what ORMs (Prisma/quaint) query:
		//   typname, typtype ("char" OID 18), typelem (oid), typbasetype (oid),
		//   typrelid (oid), typcategory.
		// typtype is the "char" single-byte type (OID 18), NOT text (OID 25).
		sch := &tableSchema{Name: "pg_type", PKIndex: -1, Cols: []colMeta{
			pgOidCol("oid"), textCol("typname"), pgOidCol("typnamespace"),
			charCol("typtype"), pgOidCol("typrelid"), textCol("typcategory"),
			pgOidCol("typelem"), pgOidCol("typbasetype"),
		}}
		var rows [][]Datum
		// OID 0 pseudo-type: returned when clients look up the "null/no type" OID.
		rows = append(rows, []Datum{
			oidDatum(0), {Text: "unknown"}, oidDatum(11),
			{Text: "p"}, oidDatum(0), {Text: "X"},
			oidDatum(0), oidDatum(0),
		})
		// text[] (OID 1009) — needed for ANY($1) array parameters.
		rows = append(rows, []Datum{
			oidDatum(OIDTextArr), {Text: "_text"}, oidDatum(11),
			{Text: "b"}, oidDatum(0), {Text: "A"},
			oidDatum(OIDText), oidDatum(0), // typelem=25 (text), typbasetype=0
		})
		for _, oid := range catalogTypeOIDs {
			rows = append(rows, []Datum{
				oidDatum(oid), {Text: udtName(oid)}, oidDatum(11),
				{Text: "b"}, oidDatum(0), {Text: typeCategory(oid)},
				oidDatum(0), oidDatum(0), // typelem=0, typbasetype=0 for all base types
			})
		}
		return sch, rows, true, nil

	case sn == "pg_catalog" && (tn == "pg_settings"):
		sch := &tableSchema{Name: "pg_settings", PKIndex: -1, Cols: []colMeta{
			textCol("name"), textCol("setting"), textCol("category"),
		}}
		var rows [][]Datum
		for _, n := range settingNames {
			rows = append(rows, []Datum{{Text: n}, {Text: gucValue(n)}, {Text: "BasuyuDB"}})
		}
		return sch, rows, true, nil

	case sn == "pg_catalog" && tn == "pg_indexes":
		sch := &tableSchema{Name: "pg_indexes", PKIndex: -1, Cols: []colMeta{
			textCol("schemaname"), textCol("tablename"), textCol("indexname"), textCol("indexdef"),
		}}
		var rows [][]Datum
		for _, t := range tables {
			defs, _ := e.loadIndexes(ctx, txn, sess, t.Name)
			for _, d := range defs {
				rows = append(rows, []Datum{{Text: "public"}, {Text: t.Name}, {Text: d.Name},
					{Text: "CREATE INDEX " + d.Name + " ON " + t.Name}})
			}
		}
		return sch, rows, true, nil

	case sn == "pg_catalog" && tn == "pg_constraint":
		sch := &tableSchema{Name: "pg_constraint", PKIndex: -1, Cols: []colMeta{
			pgOidCol("oid"), textCol("conname"), pgOidCol("conrelid"), charCol("contype"),
			boolCol("condeferrable"), boolCol("condeferred"),
		}}
		var rows [][]Datum
		for _, t := range tables {
			if t.PKIndex >= 0 {
				rows = append(rows, []Datum{oidDatum(tableOID(t.Name + "_pkey")), {Text: t.Name + "_pkey"},
					oidDatum(tableOID(t.Name)), {Text: "p"}, boolDatum(false), boolDatum(false)})
			}
			for _, c := range t.Cols {
				if c.FKTable != "" {
					rows = append(rows, []Datum{oidDatum(tableOID(t.Name + "_" + c.Name + "_fkey")),
						{Text: t.Name + "_" + c.Name + "_fkey"}, oidDatum(tableOID(t.Name)), {Text: "f"},
						boolDatum(false), boolDatum(false)})
				}
			}
		}
		return sch, rows, true, nil

	case sn == "pg_catalog" && tn == "pg_enum":
		sch := &tableSchema{Name: "pg_enum", PKIndex: -1, Cols: []colMeta{
			pgOidCol("oid"), pgOidCol("enumtypid"), textCol("enumlabel"),
			{Name: "enumsortorder", TypeOID: OIDFloat4},
		}}
		enums := e.catalogListEnums(ctx, txn, sess)
		var enumRows [][]Datum
		for _, en := range enums {
			typeOID := tableOID(en[0])
			for i := 1; i < len(en); i++ {
				label := en[i]
				rowOID := tableOID(en[0] + "_" + label)
				enumRows = append(enumRows, []Datum{
					oidDatum(rowOID),
					oidDatum(typeOID),
					{Text: label},
					{Text: strconv.FormatFloat(float64(i), 'f', -1, 64)},
				})
			}
		}
		return sch, enumRows, true, nil

	case sn == "pg_catalog" && tn == "pg_index":
		// pg_index: index metadata. Used by some ORM introspection queries.
		sch := &tableSchema{Name: "pg_index", PKIndex: -1, Cols: []colMeta{
			pgOidCol("indexrelid"), pgOidCol("indrelid"), boolCol("indisunique"),
			boolCol("indisprimary"), boolCol("indisvalid"), boolCol("indisready"),
			textCol("indpred"),
		}}
		var rows [][]Datum
		// Synthesize index entries from our stored index definitions.
		for _, t := range tables {
			defs, _ := e.loadIndexes(ctx, txn, sess, t.Name)
			for _, d := range defs {
				isPrimary := (t.PKIndex >= 0 && d.Name == t.Name+"_pkey")
				rows = append(rows, []Datum{
					oidDatum(tableOID(d.Name)), oidDatum(tableOID(t.Name)),
					boolDatum(d.Unique || isPrimary), boolDatum(isPrimary),
					boolDatum(true), boolDatum(true), {Null: true},
				})
			}
		}
		return sch, rows, true, nil

	case sn == "pg_catalog" && tn == "pg_am":
		// pg_am: access methods (btree, hash, etc.). Some queries check this.
		sch := &tableSchema{Name: "pg_am", PKIndex: -1, Cols: []colMeta{
			pgOidCol("oid"), textCol("amname"),
		}}
		rows := [][]Datum{
			{oidDatum(403), {Text: "btree"}},
			{oidDatum(405), {Text: "hash"}},
		}
		return sch, rows, true, nil

	case sn == "pg_catalog" && tn == "pg_attrdef":
		// pg_attrdef: column default values. BasuyuDB stores defaults in table schema;
		// this synthetic view exposes them for introspection queries.
		sch := &tableSchema{Name: "pg_attrdef", PKIndex: -1, Cols: []colMeta{
			pgOidCol("oid"), pgOidCol("adrelid"), intCol("adnum"), textCol("adbin"),
		}}
		var rows [][]Datum
		for _, t := range tables {
			for i, c := range t.Cols {
				if c.Default != nil && c.Default.Text != "" {
					rows = append(rows, []Datum{
						oidDatum(tableOID(t.Name+"_col_"+c.Name+"_def")),
						oidDatum(tableOID(t.Name)),
						{Text: itoaInt(i + 1)},
						{Text: c.Default.Text},
					})
				}
			}
		}
		return sch, rows, true, nil

	case sn == "pg_catalog" && tn == "pg_extension":
		// pg_extension: installed extensions. Return empty — no extensions.
		sch := &tableSchema{Name: "pg_extension", PKIndex: -1, Cols: []colMeta{
			pgOidCol("oid"), textCol("extname"), pgOidCol("extnamespace"),
		}}
		return sch, nil, true, nil

	case sn == "pg_catalog" && tn == "pg_proc":
		// pg_proc: user-defined functions. BasuyuDB has none yet.
		sch := &tableSchema{Name: "pg_proc", PKIndex: -1, Cols: []colMeta{
			pgOidCol("oid"), textCol("proname"), pgOidCol("pronamespace"),
			pgOidCol("prorettype"), {Name: "pronargs", TypeOID: OIDInt2},
			boolCol("proretset"), charCol("provolatile"),
			textCol("prosrc"), textCol("proargtypes"),
		}}
		return sch, nil, true, nil

	case sn == "pg_catalog" && tn == "pg_roles":
		sch := &tableSchema{Name: "pg_roles", PKIndex: -1, Cols: []colMeta{
			pgOidCol("oid"), textCol("rolname"), boolCol("rolsuper"),
			boolCol("rolinherit"), boolCol("rolcreaterole"), boolCol("rolcreatedb"),
			boolCol("rolcanlogin"), boolCol("rolreplication"), boolCol("rolbypassrls"),
			intCol("rolconnlimit"), textCol("rolpassword"),
			{Name: "rolvaliduntil", TypeOID: OIDTimestamptz},
		}}
		rows := [][]Datum{
			{oidDatum(10), {Text: "postgres"}, boolDatum(true), boolDatum(true),
				boolDatum(true), boolDatum(true), boolDatum(true), boolDatum(false),
				boolDatum(false), {Text: "-1"}, {Null: true}, {Null: true}},
		}
		return sch, rows, true, nil

	case sn == "pg_catalog" && tn == "pg_user":
		sch := &tableSchema{Name: "pg_user", PKIndex: -1, Cols: []colMeta{
			textCol("usename"), pgOidCol("usesysid"), boolCol("usecreatedb"),
			boolCol("usesuper"), boolCol("userepl"), boolCol("usebypassrls"),
			textCol("passwd"), {Name: "valuntil", TypeOID: OIDTimestamptz},
			{Name: "useconfig", TypeOID: OIDTextArr},
		}}
		rows := [][]Datum{
			{{Text: "postgres"}, oidDatum(10), boolDatum(true), boolDatum(true),
				boolDatum(false), boolDatum(false), {Null: true}, {Null: true}, {Null: true}},
		}
		return sch, rows, true, nil

	case sn == "pg_catalog" && (tn == "pg_stat_user_tables" || tn == "pg_statio_user_tables"):
		sch := &tableSchema{Name: tn, PKIndex: -1, Cols: []colMeta{
			pgOidCol("relid"), textCol("schemaname"), textCol("relname"),
			{Name: "seq_scan", TypeOID: OIDInt8}, {Name: "seq_tup_read", TypeOID: OIDInt8},
			{Name: "idx_scan", TypeOID: OIDInt8}, {Name: "idx_tup_fetch", TypeOID: OIDInt8},
			{Name: "n_tup_ins", TypeOID: OIDInt8}, {Name: "n_tup_upd", TypeOID: OIDInt8},
			{Name: "n_tup_del", TypeOID: OIDInt8}, {Name: "n_tup_hot_upd", TypeOID: OIDInt8},
			{Name: "n_live_tup", TypeOID: OIDInt8}, {Name: "n_dead_tup", TypeOID: OIDInt8},
			{Name: "last_vacuum", TypeOID: OIDTimestamptz},
			{Name: "last_autovacuum", TypeOID: OIDTimestamptz},
			{Name: "last_analyze", TypeOID: OIDTimestamptz},
			{Name: "last_autoanalyze", TypeOID: OIDTimestamptz},
		}}
		var rows [][]Datum
		for _, t := range tables {
			rows = append(rows, []Datum{
				oidDatum(tableOID(t.Name)), {Text: "public"}, {Text: t.Name},
				{Text: "0"}, {Text: "0"}, {Text: "0"}, {Text: "0"},
				{Text: "0"}, {Text: "0"}, {Text: "0"}, {Text: "0"},
				{Text: "0"}, {Text: "0"},
				{Null: true}, {Null: true}, {Null: true}, {Null: true},
			})
		}
		return sch, rows, true, nil

	case sn == "pg_catalog" && tn == "pg_locks":
		sch := &tableSchema{Name: "pg_locks", PKIndex: -1, Cols: []colMeta{
			textCol("locktype"), pgOidCol("database"), pgOidCol("relation"),
			intCol("page"), {Name: "tuple", TypeOID: OIDInt2},
			textCol("virtualxid"), pgOidCol("transactionid"),
			pgOidCol("classid"), pgOidCol("objid"),
			{Name: "objsubid", TypeOID: OIDInt2},
			textCol("virtualtransaction"), intCol("pid"),
			textCol("mode"), boolCol("granted"), boolCol("fastpath"),
			{Name: "waitstart", TypeOID: OIDTimestamptz},
		}}
		return sch, nil, true, nil

	case sn == "pg_catalog" && (tn == "pg_description" || tn == "pg_shdescription"):
		sch := &tableSchema{Name: tn, PKIndex: -1, Cols: []colMeta{
			pgOidCol("objoid"), pgOidCol("classoid"), intCol("objsubid"), textCol("description"),
		}}
		return sch, nil, true, nil

	case sn == "pg_catalog" && tn == "pg_stat_activity":
		sch := &tableSchema{Name: "pg_stat_activity", PKIndex: -1, Cols: []colMeta{
			pgOidCol("datid"), textCol("datname"), intCol("pid"),
			pgOidCol("usesysid"), textCol("usename"), textCol("application_name"),
			textCol("client_addr"), textCol("state"), textCol("query"),
			{Name: "query_start", TypeOID: OIDTimestamptz},
			{Name: "state_change", TypeOID: OIDTimestamptz},
			textCol("wait_event_type"), textCol("wait_event"), textCol("backend_type"),
		}}
		return sch, nil, true, nil

	case sn == "pg_catalog" && tn == "pg_sequences":
		seqNames := e.catalogListSequences(ctx, txn, sess)
		sch := &tableSchema{Name: "pg_sequences", PKIndex: -1, Cols: []colMeta{
			textCol("schemaname"), textCol("sequencename"), textCol("sequenceowner"),
			textCol("data_type"),
			{Name: "start_value", TypeOID: OIDInt8}, {Name: "min_value", TypeOID: OIDInt8},
			{Name: "max_value", TypeOID: OIDInt8}, {Name: "increment_by", TypeOID: OIDInt8},
			boolCol("cycle"), {Name: "cache_size", TypeOID: OIDInt8},
			{Name: "last_value", TypeOID: OIDInt8},
		}}
		var rows [][]Datum
		for _, name := range seqNames {
			rows = append(rows, []Datum{
				{Text: "public"}, {Text: name}, {Text: "postgres"},
				{Text: "bigint"}, {Text: "1"}, {Text: "1"},
				{Text: "9223372036854775807"}, {Text: "1"},
				boolDatum(false), {Text: "1"}, {Null: true},
			})
		}
		return sch, rows, true, nil

	case sn == "pg_catalog" && tn == "pg_views":
		// Override the earlier stub with a live-data version that reads stored views.
		viewList := e.catalogListViews(ctx, txn, sess)
		sch := &tableSchema{Name: "pg_views", PKIndex: -1, Cols: []colMeta{
			textCol("schemaname"), textCol("viewname"), textCol("viewowner"), textCol("definition"),
		}}
		var rows [][]Datum
		for _, v := range viewList {
			rows = append(rows, []Datum{{Text: "public"}, {Text: v[0]}, {Text: "postgres"}, {Text: v[1]}})
		}
		return sch, rows, true, nil

	// ── information_schema extras ─────────────────────────────────────────────

	case sn == "information_schema" && tn == "referential_constraints":
		sch := &tableSchema{Name: "referential_constraints", PKIndex: -1, Cols: []colMeta{
			textCol("constraint_catalog"), textCol("constraint_schema"), textCol("constraint_name"),
			textCol("unique_constraint_catalog"), textCol("unique_constraint_schema"),
			textCol("unique_constraint_name"),
			textCol("match_option"), textCol("update_rule"), textCol("delete_rule"),
		}}
		return sch, nil, true, nil

	case sn == "information_schema" && tn == "key_column_usage":
		sch := &tableSchema{Name: "key_column_usage", PKIndex: -1, Cols: []colMeta{
			textCol("constraint_catalog"), textCol("constraint_schema"), textCol("constraint_name"),
			textCol("table_catalog"), textCol("table_schema"), textCol("table_name"),
			textCol("column_name"), intCol("ordinal_position"),
			{Name: "position_in_unique_constraint", TypeOID: OIDInt4},
		}}
		return sch, nil, true, nil

	case sn == "information_schema" && tn == "constraint_column_usage":
		sch := &tableSchema{Name: "constraint_column_usage", PKIndex: -1, Cols: []colMeta{
			textCol("table_catalog"), textCol("table_schema"), textCol("table_name"),
			textCol("column_name"),
			textCol("constraint_catalog"), textCol("constraint_schema"), textCol("constraint_name"),
		}}
		return sch, nil, true, nil

	case sn == "information_schema" && tn == "table_constraints":
		sch := &tableSchema{Name: "table_constraints", PKIndex: -1, Cols: []colMeta{
			textCol("constraint_catalog"), textCol("constraint_schema"), textCol("constraint_name"),
			textCol("table_catalog"), textCol("table_schema"), textCol("table_name"),
			textCol("constraint_type"), textCol("is_deferrable"),
			textCol("initially_deferred"), textCol("enforced"),
		}}
		return sch, nil, true, nil

	case sn == "information_schema" && (tn == "role_table_grants" || tn == "role_column_grants"):
		sch := &tableSchema{Name: tn, PKIndex: -1, Cols: []colMeta{
			textCol("grantor"), textCol("grantee"),
			textCol("table_catalog"), textCol("table_schema"), textCol("table_name"),
			textCol("privilege_type"), textCol("is_grantable"),
		}}
		return sch, nil, true, nil

	case sn == "information_schema" && tn == "applicable_roles":
		sch := &tableSchema{Name: "applicable_roles", PKIndex: -1, Cols: []colMeta{
			textCol("grantee"), textCol("role_name"), textCol("is_grantable"),
		}}
		rows := [][]Datum{{{Text: "postgres"}, {Text: "postgres"}, {Text: "YES"}}}
		return sch, rows, true, nil

	case sn == "information_schema" && tn == "enabled_roles":
		sch := &tableSchema{Name: "enabled_roles", PKIndex: -1, Cols: []colMeta{
			textCol("role_name"),
		}}
		rows := [][]Datum{{{Text: "postgres"}}}
		return sch, rows, true, nil

	case sn == "information_schema" && tn == "role_usage_grants":
		sch := &tableSchema{Name: "role_usage_grants", PKIndex: -1, Cols: []colMeta{
			textCol("grantor"), textCol("grantee"),
			textCol("object_catalog"), textCol("object_schema"),
			textCol("object_name"), textCol("object_type"),
			textCol("privilege_type"), textCol("is_grantable"),
		}}
		return sch, nil, true, nil
	}
	return nil, nil, false, nil
}

// catalogListSequences returns the names of sequences stored under the
// "#seq#NAME" schema-key discriminator for the session's namespace.
func (e *execImpl) catalogListSequences(ctx context.Context, txn *transactions.Txn, sess *session.Session) []string {
	prefix := e.store.Encoder().SchemaKey(sess.Namespace(), "")
	it := e.txn.NewIterator(txn, prefix)
	defer it.Close()
	plen := len(prefix.Bytes())
	const disc = "#seq#"
	var names []string
	for it.Rewind(); it.Valid(); it.Next() {
		tail := string(it.Key().Bytes()[plen:])
		if !strings.HasPrefix(tail, disc) {
			continue
		}
		names = append(names, tail[len(disc):])
	}
	return names
}

// catalogListViews returns [[viewName, definition], ...] for every view stored
// under the "#view#NAME" schema-key discriminator for the session's namespace.
func (e *execImpl) catalogListViews(ctx context.Context, txn *transactions.Txn, sess *session.Session) [][]string {
	prefix := e.store.Encoder().SchemaKey(sess.Namespace(), "")
	it := e.txn.NewIterator(txn, prefix)
	defer it.Close()
	plen := len(prefix.Bytes())
	const disc = "#view#"
	var result [][]string
	for it.Rewind(); it.Valid(); it.Next() {
		tail := string(it.Key().Bytes()[plen:])
		if !strings.HasPrefix(tail, disc) {
			continue
		}
		name := tail[len(disc):]
		raw, err := it.Value()
		if err != nil {
			continue
		}
		result = append(result, []string{name, string(raw)})
	}
	return result
}

// catalogListEnums returns [[typeName, val1, val2, ...], ...] for each enum type
// stored under the "#enum#NAME" discriminator in the session's namespace.
func (e *execImpl) catalogListEnums(ctx context.Context, txn *transactions.Txn, sess *session.Session) [][]string {
	prefix := e.store.Encoder().SchemaKey(sess.Namespace(), "")
	it := e.txn.NewIterator(txn, prefix)
	defer it.Close()
	plen := len(prefix.Bytes())
	const disc = "#enum#"
	var result [][]string
	for it.Rewind(); it.Valid(); it.Next() {
		tail := string(it.Key().Bytes()[plen:])
		if !strings.HasPrefix(tail, disc) {
			continue
		}
		name := tail[len(disc):]
		raw, err := it.Value()
		if err != nil || len(raw) == 0 {
			continue
		}
		entry := []string{name}
		for _, lbl := range strings.Split(string(raw), ",") {
			if lbl != "" {
				entry = append(entry, lbl)
			}
		}
		result = append(result, entry)
	}
	return result
}

// oidPublicSchema is the synthetic OID of the "public" schema, matching the value
// PostgreSQL uses so introspection joins line up.
const oidPublicSchema = 2200

func oidCol(name string) colMeta   { return colMeta{Name: name, TypeOID: OIDInt8} }
func pgOidCol(name string) colMeta { return colMeta{Name: name, TypeOID: OIDOid} }
func charCol(name string) colMeta  { return colMeta{Name: name, TypeOID: OIDChar} }
func boolCol(name string) colMeta  { return colMeta{Name: name, TypeOID: OIDBool} }
func oidDatum(v uint32) Datum     { return Datum{Text: itoaInt(int(v))} }
func boolDatum(b bool) Datum {
	if b {
		return Datum{Text: "t"}
	}
	return Datum{Text: "f"}
}

// tableOID derives a stable synthetic OID for a relation/constraint name (FNV-1a
// into the user-OID range) so pg_class/pg_attribute/pg_constraint joins are
// self-consistent within a session.
func tableOID(name string) uint32 {
	const offset, prime = 2166136261, 16777619
	h := uint32(offset)
	for i := 0; i < len(name); i++ {
		h ^= uint32(name[i])
		h *= prime
	}
	return 16384 + h%1000000
}

// catalogTypeOIDs are the type OIDs BasuyuDB surfaces via pg_type.
//
// CRITICAL: this list must include every OID that appears in the TypeOID of
// any pg_catalog RowDescription column. If quaint/Prisma schema-engine receives a
// RowDescription with a TypeOID it doesn't recognise, it issues a pg_type lookup
// for that OID. If we return 0 rows for that lookup, quaint loops infinitely
// (re-preparing the same query with a new statement name until it crashes).
//
// So we MUST include:
//   OIDChar  (18)  — used as typtype column type in pg_type itself
//   OIDOid   (26)  — used as oid/typelem/typbasetype/typrelid column types
//   OIDName  (19)  — used by pg_namespace.nspname, pg_class.relname, etc.
//   OIDVoid  (2278)— used by functions returning void (e.g. set_config)
var (
	OIDName uint32 = 19
	OIDVoid uint32 = 2278
)

var catalogTypeOIDs = []uint32{
	// Internal meta-types that appear in RowDescriptions for catalog queries.
	// Must come first so quaint resolves them during its type-bootstrap loop.
	OIDChar,   // 18 — "char" (1-byte internal type; typtype column in pg_type)
	OIDName,   // 19 — name (63-byte identifier type; relname, nspname etc.)
	OIDOid,    // 26 — oid (4-byte object identifier)
	OIDVoid,   // 2278 — void (return type of set_config and similar)

	// User-facing scalar types.
	OIDBool, OIDInt2, OIDInt4, OIDInt8, OIDFloat4, OIDFloat8, OIDNumeric,
	OIDText, OIDVarchar, OIDBpchar, OIDUUID, OIDJSON, OIDJSONB, OIDBytea,
	OIDDate, OIDTime, OIDTimestamp, OIDTimestamptz,
}

// settingNames are the GUCs exposed via pg_settings (values from gucValue).
var settingNames = []string{
	"server_version", "server_version_num", "transaction_isolation",
	"standard_conforming_strings", "client_encoding", "TimeZone",
	"search_path", "DateStyle", "integer_datetimes", "bytea_output",
	"max_identifier_length",
}

// typeCategory returns the pg_type typcategory code for a supported OID.
func typeCategory(oid uint32) string {
	switch oid {
	case OIDBool:
		return "B"
	case OIDInt2, OIDInt4, OIDInt8, OIDFloat4, OIDFloat8, OIDNumeric:
		return "N"
	case OIDDate, OIDTime, OIDTimestamp, OIDTimestamptz:
		return "D"
	case OIDOid, OIDChar, OIDName:
		return "U" // pseudo / system types
	case OIDVoid:
		return "P" // pseudo
	default:
		return "S"
	}
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
