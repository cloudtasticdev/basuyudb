package executor

import (
	"context"
	"strings"

	"github.com/cloudtasticdev/basuyudb/engine/internal/ast"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
)

// gucValue returns the value a SHOW / current_setting() reports for a run-time
// configuration parameter. BasuyuDB advertises PostgreSQL-15 wire semantics;
// these are the values drivers and ORMs probe on connect. Unknown parameters
// return "" so a client that checks optional GUCs does not error out.
func gucValue(name string) string {
	v, _ := gucValueKnown(name)
	return v
}

// gucValueKnown returns the built-in default for a run-time configuration
// parameter and whether the parameter is one BasuyuDB recognizes. Unknown
// parameters return ("", false) so current_setting() can honor missing_ok and
// raise SQLSTATE 42704 otherwise (matching PostgreSQL). Custom namespaced GUCs
// (containing a dot, e.g. app.current_tenant) are not "known" defaults — they
// exist only once SET, and are read from the session settings by the caller.
func gucValueKnown(name string) (string, bool) {
	switch normalizeGUC(name) {
	case "server_version":
		return "15.0", true
	case "server_version_num":
		return "150000", true
	case "transaction_isolation", "default_transaction_isolation":
		return "read committed", true
	case "transaction_read_only", "default_transaction_read_only":
		return "off", true
	case "standard_conforming_strings":
		return "on", true
	case "client_encoding", "server_encoding":
		return "UTF8", true
	case "timezone":
		return "UTC", true
	case "search_path":
		return "public", true
	case "datestyle":
		return "ISO, MDY", true
	case "integer_datetimes":
		return "on", true
	case "intervalstyle":
		return "postgres", true
	case "bytea_output":
		return "hex", true
	case "max_identifier_length":
		return "63", true
	case "is_superuser":
		return "off", true
	case "in_hot_standby":
		return "off", true
	case "application_name":
		return "", true
	default:
		return "", false
	}
}

// normalizeGUC lowercases and collapses the multi-word forms ("TIME ZONE").
func normalizeGUC(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	switch n {
	case "time zone":
		return "timezone"
	}
	return n
}

// niladicKeyword resolves the SQL keyword-functions that may be written without
// parentheses (PostgreSQL allows `SELECT current_user`, `CURRENT_TIMESTAMP`,
// etc.). Returns ok=false for anything that is an ordinary identifier.
func niladicKeyword(name string) (value, bool) {
	switch strings.ToLower(name) {
	case "current_user", "session_user", "user", "current_role":
		return value{text: "postgres", oid: OIDText}, true
	case "current_catalog", "current_database":
		return value{text: "defaultdb", oid: OIDText}, true
	case "current_schema":
		return value{text: "public", oid: OIDText}, true
	case "current_timestamp", "now", "transaction_timestamp", "statement_timestamp", "clock_timestamp":
		return value{text: nowText(), oid: OIDTimestamptz}, true
	case "current_date":
		return value{text: nowText()[:10], oid: OIDDate}, true
	case "current_time":
		return value{text: nowTimeText(), oid: OIDTime}, true
	case "localtime", "localtimestamp":
		return value{text: nowText(), oid: OIDTimestamptz}, true
	}
	return value{}, false
}

// execShow implements SHOW <name>: a single row whose column is the parameter
// name and whose value is the configured setting.
func (e *execImpl) execShow(ctx context.Context, s *ast.ShowStmt, sess *session.Session) (*Result, error) {
	col := normalizeGUC(s.Name)
	val := gucValue(s.Name)
	// A session-set GUC (SET / set_config) overrides the built-in default and is
	// the only source for custom namespaced parameters (e.g. app.current_tenant).
	if sess != nil {
		if v, ok := sess.GetSetting(s.Name); ok {
			val = v
		}
	}
	return &Result{
		Columns: []Column{{Name: col, TypeOID: OIDText}},
		Rows:    [][]Datum{{{Text: val}}},
		Command: "SHOW",
	}, nil
}
