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
	case "#>>", "#>":
		// #>> extracts at a path AS text; #> extracts at a path AS json (jsonb).
		path := parsePGPath(key.text)
		cur := parsed
		for _, step := range path {
			next, ok := jsonStep(cur, step)
			if !ok {
				return value{null: true, oid: OIDText}, nil
			}
			cur = next
		}
		if op == "#>" {
			v := jsonResult(cur, false)
			v.oid = OIDJSONB
			return v, nil
		}
		return jsonResult(cur, true), nil
	}
	return value{}, newExecError("0A000", "unknown JSON operator %q", op)
}

// jsonbDelete implements the JSONB `-` delete operator:
//
//	jsonb - text   : remove an object key (no-op on arrays unless value matches)
//	jsonb - int    : remove an array element by index (negative counts from end)
//	jsonb - text[] : remove multiple object keys
//
// rkey is the right-hand operand value; rIsInt reports whether it should be
// treated as an integer index. Returns the modified document as jsonb.
func jsonbDelete(doc, rkey value, rIsInt bool) (value, error) {
	if doc.null {
		return value{null: true, oid: OIDJSONB}, nil
	}
	var parsed interface{}
	if err := json.Unmarshal([]byte(doc.text), &parsed); err != nil {
		return value{}, newExecError("22023", "invalid JSON in JSONB - operand: %v", err)
	}
	if rkey.null {
		return value{text: doc.text, oid: OIDJSONB}, nil
	}

	// text[] form: remove each named key.
	if rkey.oid == OIDTextArr || (len(strings.TrimSpace(rkey.text)) >= 2 &&
		strings.HasPrefix(strings.TrimSpace(rkey.text), "{") && !rIsInt) {
		keys := pgArrayElements(rkey.text)
		if obj, ok := parsed.(map[string]interface{}); ok {
			for _, k := range keys {
				delete(obj, k)
			}
			b, _ := json.Marshal(obj)
			return value{text: string(b), oid: OIDJSONB}, nil
		}
		return value{text: doc.text, oid: OIDJSONB}, nil
	}

	switch t := parsed.(type) {
	case map[string]interface{}:
		delete(t, rkey.text)
		b, _ := json.Marshal(t)
		return value{text: string(b), oid: OIDJSONB}, nil
	case []interface{}:
		if rIsInt {
			idx, err := strconv.Atoi(strings.TrimSpace(rkey.text))
			if err == nil {
				if idx < 0 {
					idx += len(t)
				}
				if idx >= 0 && idx < len(t) {
					t = append(t[:idx], t[idx+1:]...)
				}
			}
		} else {
			// jsonb - text on an array removes matching string elements.
			out := t[:0]
			for _, el := range t {
				if s, ok := el.(string); ok && s == rkey.text {
					continue
				}
				out = append(out, el)
			}
			t = out
		}
		b, _ := json.Marshal(t)
		return value{text: string(b), oid: OIDJSONB}, nil
	default:
		return value{text: doc.text, oid: OIDJSONB}, nil
	}
}

// jsonbSet implements jsonb_set(target, path, new_value [, create_missing]).
// It sets the value at the path; when create_missing is true (default) missing
// object keys are created. Returns the modified document as jsonb.
func jsonbSet(target value, pathText string, newVal value, createMissing bool) (value, error) {
	if target.null {
		return value{null: true, oid: OIDJSONB}, nil
	}
	var parsed interface{}
	if err := json.Unmarshal([]byte(target.text), &parsed); err != nil {
		return value{}, newExecError("22023", "invalid JSON in jsonb_set target: %v", err)
	}
	var nv interface{}
	if newVal.null {
		nv = nil
	} else if err := json.Unmarshal([]byte(newVal.text), &nv); err != nil {
		// Treat a non-JSON new_value as a JSON string.
		nv = newVal.text
	}
	path := parsePGPath(pathText)
	if len(path) == 0 {
		return value{text: target.text, oid: OIDJSONB}, nil
	}
	updated, ok := jsonSetPath(parsed, path, nv, createMissing)
	if !ok {
		return value{text: target.text, oid: OIDJSONB}, nil
	}
	b, _ := json.Marshal(updated)
	return value{text: string(b), oid: OIDJSONB}, nil
}

// jsonSetPath recursively sets new_value at path within cur, returning the
// (possibly replaced) container and whether the set succeeded.
func jsonSetPath(cur interface{}, path []string, newVal interface{}, createMissing bool) (interface{}, bool) {
	if len(path) == 0 {
		return newVal, true
	}
	step := path[0]
	switch t := cur.(type) {
	case map[string]interface{}:
		child, exists := t[step]
		if !exists {
			if !createMissing {
				return cur, false
			}
			if len(path) == 1 {
				t[step] = newVal
				return t, true
			}
			child = map[string]interface{}{}
		}
		nc, ok := jsonSetPath(child, path[1:], newVal, createMissing)
		if !ok {
			return cur, false
		}
		t[step] = nc
		return t, true
	case []interface{}:
		idx, err := strconv.Atoi(step)
		if err != nil {
			return cur, false
		}
		if idx < 0 {
			idx += len(t)
		}
		if idx < 0 || idx >= len(t) {
			if createMissing && idx >= len(t) {
				// Append at the end for out-of-range positive index.
				if len(path) == 1 {
					return append(t, newVal), true
				}
			}
			return cur, false
		}
		nc, ok := jsonSetPath(t[idx], path[1:], newVal, createMissing)
		if !ok {
			return cur, false
		}
		t[idx] = nc
		return t, true
	default:
		return cur, false
	}
}

// jsonbArrayElements parses a JSONB array document and returns each element as a
// value. asText=true renders scalars unquoted (jsonb_array_elements_text);
// asText=false renders each element as jsonb. Returns an error if the document
// is not a JSON array.
func jsonbArrayElements(doc value, asText bool) ([]value, error) {
	if doc.null {
		return nil, nil
	}
	var parsed interface{}
	if err := json.Unmarshal([]byte(doc.text), &parsed); err != nil {
		return nil, newExecError("22023", "invalid JSON in jsonb_array_elements: %v", err)
	}
	arr, ok := parsed.([]interface{})
	if !ok {
		return nil, newExecError("22023", "cannot extract elements from a non-array")
	}
	out := make([]value, 0, len(arr))
	for _, el := range arr {
		v := jsonResult(el, asText)
		if !asText {
			v.oid = OIDJSONB
		}
		out = append(out, v)
	}
	return out, nil
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
