package audit

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"go.kenn.io/docbank/internal/jsontext"
)

// MarshalJSONRecord returns the deterministic metadata-v1 JSON form of one
// registered canonical audit record. Byte fields use unpadded base64url;
// their registered type keeps them distinct from ordinary text.
func MarshalJSONRecord(record Record) (json.RawMessage, error) {
	if err := Validate(record); err != nil {
		return nil, err
	}
	value, err := portableRecordValue(record, 0)
	if err != nil {
		return nil, err
	}
	var encoded bytes.Buffer
	enc := json.NewEncoder(&encoded)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, fmt.Errorf("encoding portable audit record: %w", err)
	}
	return bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'}), nil
}

// UnmarshalJSONRecord parses one deterministic metadata-v1 JSON audit record,
// restoring its typed canonical values and enforcing the closed registry.
func UnmarshalJSONRecord(raw json.RawMessage) (Record, error) {
	if err := jsontext.Validate(raw, "audit record JSON"); err != nil {
		return Record{}, err
	}
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
		return json.RawMessage("null"), nil
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

func parsePortableRecord(raw json.RawMessage, depth int) (Record, error) {
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

func parsePortableValue(raw json.RawMessage, rule valueRule, depth int) (Value, error) {
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

func decodeJSONObject(raw json.RawMessage, subject string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	token, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("decoding %s: %w", subject, err)
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("%s must be a JSON object", subject)
	}
	fields := make(map[string]json.RawMessage)
	for dec.More() {
		nameToken, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("decoding %s field name: %w", subject, err)
		}
		name, ok := nameToken.(string)
		if !ok {
			return nil, fmt.Errorf("%s field name must be a string", subject)
		}
		if _, exists := fields[name]; exists {
			return nil, fmt.Errorf("%s contains duplicate field %q", subject, name)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, fmt.Errorf("decoding %s field %q: %w", subject, name, err)
		}
		fields[name] = value
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("closing %s: %w", subject, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s contains trailing JSON", subject)
	}
	return fields, nil
}

func decodeJSONArray(raw json.RawMessage) ([]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var values []json.RawMessage
	if err := dec.Decode(&values); err != nil {
		return nil, fmt.Errorf("audit list must be a JSON array: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("audit list contains trailing JSON")
	}
	return values, nil
}

func decodeJSONScalar(raw json.RawMessage, value any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(value); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("JSON scalar contains trailing data")
	}
	return nil
}

func requireObjectFields(fields map[string]json.RawMessage, subject string, expected ...string) error {
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
