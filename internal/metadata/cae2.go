package metadata

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"time"
)

const (
	caeAbsent   = byte(0x00)
	caeUnsigned = byte(0x03)
	caeBytes    = byte(0x05)
	caeTime     = byte(0x07)
	caeUUID     = byte(0x08)
	caeDigest   = byte(0x09)
)

// ProvenanceIdentity returns the normative CAE2 identity digest for a v1
// provenance fact. The stored identity field itself is deliberately excluded.
func ProvenanceIdentity(p Provenance) (string, error) {
	ingestID, err := parseUUID(p.IngestID)
	if err != nil {
		return "", fmt.Errorf("encoding provenance identity: %w", err)
	}
	if p.NodeID <= 0 {
		return "", fmt.Errorf("encoding provenance identity: invalid node ID %d", p.NodeID)
	}
	var supersedes *[32]byte
	if p.Supersedes != nil {
		parsed, err := parseDigest("provenance supersedes", *p.Supersedes)
		if err != nil {
			return "", err
		}
		supersedes = &parsed
	}
	if p.OriginalMTime != nil {
		parsed, err := time.Parse(timestampLayout, *p.OriginalMTime)
		if err != nil || parsed.UTC().Format(timestampLayout) != *p.OriginalMTime {
			return "", fmt.Errorf("encoding provenance identity: invalid original_mtime %q", *p.OriginalMTime)
		}
	}

	var record bytes.Buffer
	appendFrame(&record, []byte("docbank-audit"))
	appendUint(&record, FormatVersion)
	appendFrame(&record, []byte("provenance_identity"))
	appendUint(&record, 5)

	appendFrame(&record, []byte("ingest_id"))
	record.WriteByte(caeUUID)
	record.Write(ingestID[:])

	appendFrame(&record, []byte("node_id"))
	record.WriteByte(caeUnsigned)
	appendUint(&record, uint64(p.NodeID))

	appendFrame(&record, []byte("original_mtime"))
	if p.OriginalMTime == nil {
		record.WriteByte(caeAbsent)
	} else {
		record.WriteByte(caeTime)
		appendFrame(&record, []byte(*p.OriginalMTime))
	}

	appendFrame(&record, []byte("original_path"))
	if p.OriginalPath == nil {
		record.WriteByte(caeAbsent)
	} else {
		record.WriteByte(caeBytes)
		appendFrame(&record, *p.OriginalPath)
	}

	appendFrame(&record, []byte("supersedes"))
	if supersedes == nil {
		record.WriteByte(caeAbsent)
	} else {
		record.WriteByte(caeDigest)
		record.Write(supersedes[:])
	}

	digest := sha256.Sum256(record.Bytes())
	return hex.EncodeToString(digest[:]), nil
}

func appendFrame(dst *bytes.Buffer, value []byte) {
	appendUint(dst, uint64(len(value)))
	dst.Write(value)
}

func appendUint(dst *bytes.Buffer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	dst.Write(encoded[:])
}
