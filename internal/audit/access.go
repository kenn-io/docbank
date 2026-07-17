package audit

import (
	"bytes"
	"encoding/hex"
)

// FieldValue returns a cloned field value from record.
func FieldValue(record Record, name string) (Value, bool) {
	for _, field := range record.Fields {
		if field.Name == name {
			return cloneValue(field.Value), true
		}
	}
	return Value{}, false
}

// IsAbsent reports whether value is the canonical absent value.
func (value Value) IsAbsent() bool { return value.kind == kindAbsent }

// BoolValue returns a canonical boolean value.
func (value Value) BoolValue() (bool, bool) {
	switch value.kind {
	case kindFalse:
		return false, true
	case kindTrue:
		return true, true
	default:
		return false, false
	}
}

// UnsignedValue returns a canonical unsigned integer value.
func (value Value) UnsignedValue() (uint64, bool) {
	return value.unsigned, value.kind == kindUnsigned
}

// BytesValue returns a copy of a canonical byte-string value.
func (value Value) BytesValue() ([]byte, bool) {
	return bytes.Clone(value.data), value.kind == kindBytes
}

// TextValue returns a canonical text value.
func (value Value) TextValue() (string, bool) {
	return string(value.data), value.kind == kindText
}

// TimestampValue returns a canonical timestamp value.
func (value Value) TimestampValue() (string, bool) {
	return string(value.data), value.kind == kindTimestamp
}

// UUIDValue returns a canonical lowercase UUIDv4 value.
func (value Value) UUIDValue() (string, bool) {
	if value.kind != kindUUID || len(value.data) != 16 {
		return "", false
	}
	return formatUUID(value.data), true
}

// DigestValue returns a canonical lowercase SHA-256 value.
func (value Value) DigestValue() (string, bool) {
	if value.kind != kindDigest || len(value.data) != 32 {
		return "", false
	}
	return hex.EncodeToString(value.data), true
}

// ListValue returns a deep copy of a canonical list value.
func (value Value) ListValue() ([]Value, bool) {
	if value.kind != kindList {
		return nil, false
	}
	items := make([]Value, len(value.items))
	for index := range value.items {
		items[index] = cloneValue(value.items[index])
	}
	return items, true
}

// RecordValue returns a deep copy of a canonical nested record value.
func (value Value) RecordValue() (Record, bool) {
	if value.kind != kindRecord || value.record == nil {
		return Record{}, false
	}
	return cloneRecord(*value.record), true
}
