package audit

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// MarshalJSONRecord returns the deterministic metadata-v1 JSON form of one
// registered canonical audit record. Byte fields use unpadded base64url;
// their registered type keeps them distinct from ordinary text.
func MarshalJSONRecord(record Record) (jsontext.Value, error) {
	if err := Validate(record); err != nil {
		return nil, err
	}
	value, err := portableRecordValue(record, 0)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value, json.Deterministic(true), jsontext.EscapeForJS(true))
	if err != nil {
		return nil, fmt.Errorf("encoding portable audit record: %w", err)
	}
	return encoded, nil
}

// UnmarshalJSONRecord parses one deterministic metadata-v1 JSON audit record,
// restoring its typed canonical values and enforcing the closed registry.
func UnmarshalJSONRecord(raw jsontext.Value) (Record, error) {
	record, err := parsePortableRecord(raw, 0)
	if err != nil {
		return Record{}, err
	}
	if err := Validate(record); err != nil {
		return Record{}, err
	}
	return record, nil
}

func portableRecordValue(record Record, depth int) (map[string]any, error) {
	if depth > maxValueDepth {
		return nil, fmt.Errorf("portable audit record nesting exceeds %d levels", maxValueDepth)
	}
	fields := make(map[string]any, len(record.Fields))
	for _, field := range record.Fields {
		value, err := portableValue(field.Value, depth+1)
		if err != nil {
			return nil, fmt.Errorf("encoding portable audit field %s.%s: %w", record.Kind, field.Name, err)
		}
		fields[field.Name] = value
	}
	return map[string]any{"kind": record.Kind, "fields": fields}, nil
}

func portableValue(value Value, depth int) (any, error) {
	if depth > maxValueDepth {
		return nil, fmt.Errorf("portable audit value nesting exceeds %d levels", maxValueDepth)
	}
	switch value.kind {
	case kindAbsent:
		return jsontext.Value("null"), nil
	case kindFalse:
		return false, nil
	case kindTrue:
		return true, nil
	case kindUnsigned:
		return value.unsigned, nil
	case kindSigned:
		return value.signed, nil
	case kindBytes:
		return base64.RawURLEncoding.EncodeToString(value.data), nil
	case kindText, kindTimestamp:
		return string(value.data), nil
	case kindUUID:
		if len(value.data) != 16 {
			return nil, errors.New("portable audit UUID has invalid width")
		}
		return formatUUID(value.data), nil
	case kindDigest:
		if len(value.data) != 32 {
			return nil, errors.New("portable audit digest has invalid width")
		}
		return hex.EncodeToString(value.data), nil
	case kindList:
		items := make([]any, len(value.items))
		for index, item := range value.items {
			encoded, err := portableValue(item, depth+1)
			if err != nil {
				return nil, err
			}
			items[index] = encoded
		}
		return items, nil
	case kindRecord:
		if value.record == nil {
			return nil, errors.New("portable audit nested record is nil")
		}
		return portableRecordValue(*value.record, depth+1)
	default:
		return nil, fmt.Errorf("portable audit value has unknown kind %d", value.kind)
	}
}

func parsePortableRecord(raw jsontext.Value, depth int) (Record, error) {
	if depth > maxValueDepth {
		return Record{}, fmt.Errorf("portable audit record nesting exceeds %d levels", maxValueDepth)
	}
	object, err := decodeJSONObject(raw, "audit record")
	if err != nil {
		return Record{}, err
	}
	if err := requireObjectFields(object, "audit record", "kind", "fields"); err != nil {
		return Record{}, err
	}
	var kind string
	if err := decodeJSONScalar(object["kind"], &kind); err != nil {
		return Record{}, fmt.Errorf("decoding audit record kind: %w", err)
	}
	schema, ok := recordSchemas[kind]
	if !ok {
		return Record{}, fmt.Errorf("unknown metadata-v1 audit record kind %q", kind)
	}
	fields, err := decodeJSONObject(object["fields"], "audit record fields")
	if err != nil {
		return Record{}, err
	}
	record := Record{Kind: kind, Fields: make([]Field, 0, len(schema.fields))}
	for _, expected := range schema.fields {
		rawValue, exists := fields[expected.name]
		if !exists {
			return Record{}, fmt.Errorf("audit record %s lacks field %q", kind, expected.name)
		}
		value, err := parsePortableValue(rawValue, expected.rule, depth+1)
		if err != nil {
			return Record{}, fmt.Errorf("decoding audit record %s.%s: %w", kind, expected.name, err)
		}
		record.Fields = append(record.Fields, Field{Name: expected.name, Value: value})
		delete(fields, expected.name)
	}
	if len(fields) != 0 {
		for field := range fields {
			return Record{}, fmt.Errorf("audit record %s contains unknown field %q", kind, field)
		}
	}
	return record, nil
}

func parsePortableValue(raw jsontext.Value, rule valueRule, depth int) (Value, error) {
	if depth > maxValueDepth {
		return Value{}, fmt.Errorf("portable audit value nesting exceeds %d levels", maxValueDepth)
	}
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		if rule.optional || rule.typeOf == typeAbsent {
			return Absent(), nil
		}
		return Value{}, fmt.Errorf("required %s value is null", rule.describe())
	}
	if rule.typeOf == typeAbsent {
		return Value{}, errors.New("absent audit value must be null")
	}
	switch rule.typeOf {
	case typeBool:
		switch string(trimmed) {
		case "false":
			return Bool(false), nil
		case "true":
			return Bool(true), nil
		default:
			return Value{}, errors.New("audit boolean must be true or false")
		}
	case typeUnsigned:
		value, err := parseCanonicalUnsigned(trimmed)
		if err != nil {
			return Value{}, err
		}
		return Unsigned(value), nil
	case typeSigned:
		value, err := parseCanonicalSigned(trimmed)
		if err != nil {
			return Value{}, err
		}
		return Signed(value), nil
	case typeBytes:
		var encoded string
		if err := decodeJSONScalar(raw, &encoded); err != nil {
			return Value{}, fmt.Errorf("audit bytes must be a base64url string: %w", err)
		}
		decoded, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != encoded {
			return Value{}, errors.New("audit bytes must use canonical unpadded base64url")
		}
		return Bytes(decoded), nil
	case typeText:
		var value string
		if err := decodeJSONScalar(raw, &value); err != nil {
			return Value{}, fmt.Errorf("audit text must be a string: %w", err)
		}
		return Text(value)
	case typeTimestamp:
		var value string
		if err := decodeJSONScalar(raw, &value); err != nil {
			return Value{}, fmt.Errorf("audit timestamp must be a string: %w", err)
		}
		return Timestamp(value)
	case typeUUID:
		var value string
		if err := decodeJSONScalar(raw, &value); err != nil {
			return Value{}, fmt.Errorf("audit UUID must be a string: %w", err)
		}
		return UUID(value)
	case typeDigest:
		var value string
		if err := decodeJSONScalar(raw, &value); err != nil {
			return Value{}, fmt.Errorf("audit digest must be a string: %w", err)
		}
		return DigestHex(value)
	case typeList:
		items, err := decodeJSONArray(raw)
		if err != nil {
			return Value{}, err
		}
		values := make([]Value, len(items))
		for index, item := range items {
			value, err := parsePortableValue(item, *rule.listElement, depth+1)
			if err != nil {
				return Value{}, fmt.Errorf("decoding audit list item %d: %w", index, err)
			}
			values[index] = value
		}
		return List(values...), nil
	case typeRecord:
		record, err := parsePortableRecord(raw, depth+1)
		if err != nil {
			return Value{}, err
		}
		return Nested(record), nil
	default:
		return Value{}, fmt.Errorf("unsupported portable audit type %d", rule.typeOf)
	}
}

func decodeJSONObject(raw jsontext.Value, subject string) (map[string]jsontext.Value, error) {
	if raw.Kind() != jsontext.KindBeginObject {
		return nil, fmt.Errorf("%s must be a JSON object", subject)
	}
	fields := make(map[string]jsontext.Value)
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", subject, err)
	}
	return fields, nil
}

func decodeJSONArray(raw jsontext.Value) ([]jsontext.Value, error) {
	if raw.Kind() != jsontext.KindBeginArray {
		return nil, errors.New("audit list must be a JSON array")
	}
	var values []jsontext.Value
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("audit list must be a JSON array: %w", err)
	}
	return values, nil
}

func decodeJSONScalar(raw jsontext.Value, value any) error {
	return json.Unmarshal(raw, value)
}

func requireObjectFields(fields map[string]jsontext.Value, subject string, expected ...string) error {
	allowed := make(map[string]bool, len(expected))
	for _, name := range expected {
		allowed[name] = true
		if _, ok := fields[name]; !ok {
			return fmt.Errorf("%s lacks field %q", subject, name)
		}
	}
	for name := range fields {
		if !allowed[name] {
			return fmt.Errorf("%s contains unknown field %q", subject, name)
		}
	}
	return nil
}

func parseCanonicalUnsigned(raw []byte) (uint64, error) {
	text := string(raw)
	if text == "" || (len(text) > 1 && text[0] == '0') || strings.IndexFunc(text, func(r rune) bool {
		return r < '0' || r > '9'
	}) >= 0 {
		return 0, errors.New("audit unsigned integer is not canonical")
	}
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("decoding audit unsigned integer: %w", err)
	}
	return value, nil
}

func parseCanonicalSigned(raw []byte) (int64, error) {
	text := string(raw)
	digits := text
	if strings.HasPrefix(digits, "-") {
		digits = digits[1:]
		if digits == "0" {
			return 0, errors.New("audit signed integer must not use negative zero")
		}
	}
	if digits == "" || (len(digits) > 1 && digits[0] == '0') || strings.IndexFunc(digits, func(r rune) bool {
		return r < '0' || r > '9'
	}) >= 0 {
		return 0, errors.New("audit signed integer is not canonical")
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("decoding audit signed integer: %w", err)
	}
	return value, nil
}

func formatUUID(value []byte) string {
	encoded := hex.EncodeToString(value)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}
