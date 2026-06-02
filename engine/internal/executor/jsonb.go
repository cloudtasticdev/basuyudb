package executor

import (
	"encoding/json"
	"strconv"
	"strings"
)

// jsonbExtract implements the JSONB extraction operators used by the OTel JOIN
// demo and general JSON columns:
//
//	value ->> key text : extract object field / array element AS text
//	value -> key json : extract object field / array element AS json
//	value #>> path text : extract at a path '{a,b,c}' AS text
//
// The left operand is the JSON document (a text/JSONB column value); the right
// operand is the key (text), array index (int as text), or path (text in
// PostgreSQL '{a,b}' form). A NULL document or a missing field yields NULL.
func jsonbExtract(op string, doc, key value) (value, error) {
	if doc.null {
		return value{null: true, oid: OIDText}, nil
	}
	var parsed interface{}
	if err := json.Unmarshal([]byte(doc.text), &parsed); err != nil {
		return value{}, newExecError("22023", "invalid JSON in JSONB %s operand: %v", op, err)
	}

	switch op {
	case "->>", "->":
		got, ok := jsonStep(parsed, key.text)
		if !ok {
			return value{null: true, oid: OIDText}, nil
		}
		return jsonResult(got, op == "->>"), nil
	case "#>>":
		path := parsePGPath(key.text)
		cur := parsed
		for _, step := range path {
			next, ok := jsonStep(cur, step)
			if !ok {
				return value{null: true, oid: OIDText}, nil
			}
			cur = next
		}
		return jsonResult(cur, true), nil
	}
	return value{}, newExecError("0A000", "unknown JSON operator %q", op)
}

// jsonStep navigates one level: object field by name, or array element by index.
func jsonStep(v interface{}, key string) (interface{}, bool) {
	switch t := v.(type) {
	case map[string]interface{}:
		got, ok := t[key]
		return got, ok
	case []interface{}:
		idx, err := strconv.Atoi(key)
		if err != nil || idx < 0 || idx >= len(t) {
			return nil, false
		}
		return t[idx], true
	default:
		return nil, false
	}
}

// jsonResult renders an extracted JSON value. asText=true renders scalars
// without quotes (->>/#>>); asText=false renders the JSON form (->).
func jsonResult(v interface{}, asText bool) value {
	if v == nil {
		return value{null: true, oid: OIDText}
	}
	if asText {
		switch t := v.(type) {
		case string:
			return value{text: t, oid: OIDText}
		case float64:
			return value{text: strconv.FormatFloat(t, 'g', -1, 64), oid: OIDText}
		case bool:
			if t {
				return value{text: "true", oid: OIDText}
			}
			return value{text: "false", oid: OIDText}
		default:
			b, _ := json.Marshal(v)
			return value{text: string(b), oid: OIDText}
		}
	}
	b, _ := json.Marshal(v)
	return value{text: string(b), oid: OIDText}
}

// parsePGPath parses a PostgreSQL text-array path like "{a,b,c}" into steps.
func parsePGPath(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}
