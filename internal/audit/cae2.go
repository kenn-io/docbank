// Package audit implements Docbank's internal audited-history authority.
package audit

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"
)

const (
	cae2Domain    = "docbank-audit"
	cae2Version   = 1
	maxValueDepth = 64
	timestampForm = "2006-01-02T15:04:05.000000000Z"
)

type valueKind byte

const (
	kindAbsent valueKind = iota
	kindFalse
	kindTrue
	kindUnsigned
	kindSigned
	kindBytes
	kindText
	kindTimestamp
	kindUUID
	kindDigest
	kindList
	kindRecord
)

// Value is one immutable CAE2 typed value. Its zero value is the canonical
// absent value.
type Value struct {
	kind     valueKind
	unsigned uint64
	signed   int64
	data     []byte
	items    []Value
	record   *Record
}

// Field is one named field in a CAE2 record. Encoding sorts fields by their
// lowercase ASCII names and rejects duplicates.
type Field struct {
	Name  string
	Value Value
}

// Record is one CAE2 record kind and its complete declared field set.
type Record struct {
	Kind   string
	Fields []Field
}

// Absent returns the canonical absent value.
func Absent() Value { return Value{} }

// Bool returns the canonical false or true value.
func Bool(value bool) Value {
	if value {
		return Value{kind: kindTrue}
	}
	return Value{kind: kindFalse}
}

// Unsigned returns an unsigned 64-bit integer value.
func Unsigned(value uint64) Value { return Value{kind: kindUnsigned, unsigned: value} }

// Signed returns a signed 64-bit two's-complement integer value.
func Signed(value int64) Value { return Value{kind: kindSigned, signed: value} }

// Bytes returns an opaque byte-string value. It copies the input.
func Bytes(value []byte) Value {
	return Value{kind: kindBytes, data: slices.Clone(value)}
}

// Text returns an exact UTF-8 text value without Unicode normalization.
func Text(value string) (Value, error) {
	if !utf8.ValidString(value) {
		return Value{}, errors.New("CAE2 text is not valid UTF-8")
	}
	return Value{kind: kindText, data: []byte(value)}, nil
}

// Timestamp returns a canonical UTC timestamp with exactly nine fractional
// digits.
func Timestamp(value string) (Value, error) {
	parsed, err := time.Parse(timestampForm, value)
	if err != nil || parsed.Format(timestampForm) != value {
		return Value{}, fmt.Errorf("CAE2 timestamp %q is not canonical UTC nanosecond form", value)
	}
	return Value{kind: kindTimestamp, data: []byte(value)}, nil
}

// UUID returns a canonical lowercase RFC 4122 UUIDv4 value.
func UUID(value string) (Value, error) {
	parsed, err := parseUUIDv4(value)
	if err != nil {
		return Value{}, err
	}
	return Value{kind: kindUUID, data: parsed[:]}, nil
}

// Digest returns a raw SHA-256 digest value.
func Digest(value [sha256.Size]byte) Value {
	return Value{kind: kindDigest, data: slices.Clone(value[:])}
}

// DigestHex returns a digest value from canonical lowercase hexadecimal.
func DigestHex(value string) (Value, error) {
	if len(value) != sha256.Size*2 || !isLowerHex(value) {
		return Value{}, errors.New("CAE2 digest must be canonical lowercase SHA-256")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return Value{}, fmt.Errorf("decoding CAE2 digest: %w", err)
	}
	return Value{kind: kindDigest, data: decoded}, nil
}

// List returns an intrinsically ordered list of complete typed values. It
// copies the values and all byte-backed children.
func List(values ...Value) Value {
	cloned := make([]Value, len(values))
	for index := range values {
		cloned[index] = cloneValue(values[index])
	}
	return Value{kind: kindList, items: cloned}
}

// Nested returns a length-framed nested CAE2 record value.
func Nested(record Record) Value {
	cloned := cloneRecord(record)
	return Value{kind: kindRecord, record: &cloned}
}

// Encode returns the canonical CAE2 binary representation of a record.
func Encode(record Record) ([]byte, error) {
	encoded := make([]byte, 0, 256)
	if err := appendRecord(&encoded, record, 0); err != nil {
		return nil, err
	}
	return encoded, nil
}

// Hash returns SHA-256 over the canonical CAE2 record bytes.
func Hash(record Record) ([sha256.Size]byte, error) {
	encoded, err := Encode(record)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

func appendRecord(dst *[]byte, record Record, depth int) error {
	if depth > maxValueDepth {
		return fmt.Errorf("CAE2 value nesting exceeds %d levels", maxValueDepth)
	}
	if !validToken(record.Kind) {
		return fmt.Errorf("invalid CAE2 record kind %q", record.Kind)
	}
	fields := slices.Clone(record.Fields)
	slices.SortFunc(fields, func(left, right Field) int {
		if left.Name < right.Name {
			return -1
		}
		if left.Name > right.Name {
			return 1
		}
		return 0
	})
	for index, field := range fields {
		if !validToken(field.Name) {
			return fmt.Errorf("invalid CAE2 field name %q in %s", field.Name, record.Kind)
		}
		if index > 0 && fields[index-1].Name == field.Name {
			return fmt.Errorf("duplicate CAE2 field %q in %s", field.Name, record.Kind)
		}
	}
	appendFrame(dst, []byte(cae2Domain))
	appendUint64(dst, cae2Version)
	appendFrame(dst, []byte(record.Kind))
	appendUint64(dst, uint64(len(fields)))
	for _, field := range fields {
		appendFrame(dst, []byte(field.Name))
		if err := appendValue(dst, field.Value, depth); err != nil {
			return fmt.Errorf("encoding CAE2 %s.%s: %w", record.Kind, field.Name, err)
		}
	}
	return nil
}

func appendValue(dst *[]byte, value Value, depth int) error {
	if depth > maxValueDepth {
		return fmt.Errorf("CAE2 value nesting exceeds %d levels", maxValueDepth)
	}
	*dst = append(*dst, byte(value.kind))
	switch value.kind {
	case kindAbsent, kindFalse, kindTrue:
		return nil
	case kindUnsigned:
		appendUint64(dst, value.unsigned)
	case kindSigned:
		if err := appendInt64(dst, value.signed); err != nil {
			return err
		}
	case kindBytes, kindText, kindTimestamp:
		appendFrame(dst, value.data)
	case kindUUID:
		if len(value.data) != 16 {
			return errors.New("invalid CAE2 UUID width")
		}
		*dst = append(*dst, value.data...)
	case kindDigest:
		if len(value.data) != sha256.Size {
			return errors.New("invalid CAE2 digest width")
		}
		*dst = append(*dst, value.data...)
	case kindList:
		appendUint64(dst, uint64(len(value.items)))
		for _, item := range value.items {
			if err := appendValue(dst, item, depth+1); err != nil {
				return err
			}
		}
	case kindRecord:
		if value.record == nil {
			return errors.New("nil CAE2 nested record")
		}
		nested := make([]byte, 0, 128)
		if err := appendRecord(&nested, *value.record, depth+1); err != nil {
			return err
		}
		appendFrame(dst, nested)
	default:
		return fmt.Errorf("unknown CAE2 value tag 0x%02x", byte(value.kind))
	}
	return nil
}

func appendFrame(dst *[]byte, value []byte) {
	appendUint64(dst, uint64(len(value)))
	*dst = append(*dst, value...)
}

func appendUint64(dst *[]byte, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	*dst = append(*dst, encoded[:]...)
}

func appendInt64(dst *[]byte, value int64) error {
	var encoded [8]byte
	if _, err := binary.Encode(encoded[:], binary.BigEndian, value); err != nil {
		return fmt.Errorf("encoding CAE2 signed integer: %w", err)
	}
	*dst = append(*dst, encoded[:]...)
	return nil
}

func validToken(value string) bool {
	if value == "" || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for index := 1; index < len(value); index++ {
		char := value[index]
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func parseUUIDv4(value string) ([16]byte, error) {
	var parsed [16]byte
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return parsed, errors.New("CAE2 UUID must be a canonical lowercase UUIDv4")
	}
	compact := make([]byte, 0, 32)
	for index := range len(value) {
		if value[index] != '-' {
			compact = append(compact, value[index])
		}
	}
	if !isLowerHex(string(compact)) {
		return parsed, errors.New("CAE2 UUID must be a canonical lowercase UUIDv4")
	}
	if _, err := hex.Decode(parsed[:], compact); err != nil {
		return parsed, fmt.Errorf("decoding CAE2 UUID: %w", err)
	}
	if parsed[6]>>4 != 4 || parsed[8]>>6 != 2 {
		return [16]byte{}, errors.New("CAE2 UUID must use version 4 and the RFC 4122 variant")
	}
	return parsed, nil
}

func isLowerHex(value string) bool {
	for index := range len(value) {
		char := value[index]
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func cloneRecord(record Record) Record {
	cloned := Record{Kind: record.Kind, Fields: make([]Field, len(record.Fields))}
	for index, field := range record.Fields {
		cloned.Fields[index] = Field{Name: field.Name, Value: cloneValue(field.Value)}
	}
	return cloned
}

func cloneValue(value Value) Value {
	cloned := value
	cloned.data = slices.Clone(value.data)
	if value.items != nil {
		cloned.items = make([]Value, len(value.items))
		for index := range value.items {
			cloned.items[index] = cloneValue(value.items[index])
		}
	}
	if value.record != nil {
		record := cloneRecord(*value.record)
		cloned.record = &record
	}
	return cloned
}
