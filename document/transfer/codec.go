package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"go.kenn.io/docbank/internal/canonical"
)

func MarshalManifestV1(value ManifestV1) ([]byte, string, error) {
	return marshalBounded(value, MaxManifestBytes, false)
}

func DecodeManifestV1(raw []byte) (ManifestV1, string, error) {
	return decodeBounded[ManifestV1](raw, MaxManifestBytes, false)
}

func MarshalPersonV1(value PersonV1) ([]byte, string, error) {
	return marshalLine(value)
}

func DecodePersonV1(raw []byte) (PersonV1, string, error) {
	return decodeLine[PersonV1](raw)
}

func MarshalSourceLineV1(value SourceLineV1) ([]byte, string, error) {
	return marshalLine(value)
}

func DecodeSourceLineV1(raw []byte) (SourceLineV1, string, error) {
	return decodeLine[SourceLineV1](raw)
}

func MarshalConversationV1(value ConversationV1) ([]byte, string, error) {
	return marshalLine(value)
}

func DecodeConversationV1(raw []byte) (ConversationV1, string, error) {
	return decodeLine[ConversationV1](raw)
}

func MarshalRecordV1(value RecordV1) ([]byte, string, error) {
	return marshalLine(value)
}

func DecodeRecordV1(raw []byte) (RecordV1, string, error) {
	return decodeLine[RecordV1](raw)
}

func MarshalCoverageV1(value CoverageV1) ([]byte, string, error) {
	return marshalLine(value)
}

func DecodeCoverageV1(raw []byte) (CoverageV1, string, error) {
	return decodeLine[CoverageV1](raw)
}

func MarshalTombstoneV1(value TombstoneV1) ([]byte, string, error) {
	return marshalLine(value)
}

func DecodeTombstoneV1(raw []byte) (TombstoneV1, string, error) {
	return decodeLine[TombstoneV1](raw)
}

func MarshalCompleteV1(value CompleteV1) ([]byte, string, error) {
	return marshalLine(value)
}

func DecodeCompleteV1(raw []byte) (CompleteV1, string, error) {
	return decodeLine[CompleteV1](raw)
}

func NormalizedRecordSHA256(value RecordV1) (string, error) {
	value.Raw = nil
	value.NormalizedSHA256 = ""
	value.Revision = ""
	value.LegacyRef = ""
	raw, err := canonical.Marshal(value)
	if err != nil {
		return "", err
	}
	return sha256Hex(raw), nil
}

func ProducerFingerprint(format string, producer ProducerV1) (string, error) {
	raw, err := canonical.Marshal([]any{format, producer.Name, producer.Version, producer.ContractRevision})
	if err != nil {
		return "", err
	}
	return sha256Hex(raw), nil
}

func SelectionDigest(selection SelectionV1) (string, error) {
	raw, err := canonical.Marshal(selection)
	if err != nil {
		return "", err
	}
	return sha256Hex(raw), nil
}

func marshalLine[T any](value T) ([]byte, string, error) {
	return marshalBounded(value, MaxRecordLineBytes, true)
}

func decodeLine[T any](raw []byte) (T, string, error) {
	return decodeBounded[T](raw, MaxRecordLineBytes, true)
}

func marshalBounded[T any](value T, limit int, countLF bool) ([]byte, string, error) {
	raw, err := canonical.Marshal(value)
	if err != nil {
		return nil, "", err
	}
	size := len(raw)
	if countLF {
		size++
	}
	if size > limit {
		if !countLF {
			return nil, "", errors.New("transfer: manifest size limit")
		}
		return nil, "", errors.New("transfer: line limit")
	}
	return raw, sha256Hex(raw), nil
}

func decodeBounded[T any](raw []byte, limit int, countLF bool) (T, string, error) {
	var zero T
	size := len(raw)
	if countLF {
		size++
	}
	if size > limit {
		if !countLF {
			return zero, "", errors.New("transfer: manifest size limit")
		}
		return zero, "", errors.New("transfer: line limit")
	}
	value, err := canonical.Decode[T](raw)
	if err != nil {
		return zero, "", err
	}
	return value, sha256Hex(raw), nil
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
