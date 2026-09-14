package store

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
)

const maxSnapshotCursorBytes = 2048

var ErrSnapshotCursor = errors.New("invalid query snapshot cursor")

type snapshotCursorPayload struct {
	Version          int    `json:"v"`
	SnapshotID       string `json:"snapshot_id"`
	QueryFingerprint string `json:"query_fingerprint"`
	SortField        string `json:"sort_field"`
	SortDirection    string `json:"sort_direction"`
	PageDirection    string `json:"page_direction"`
	PageSize         int    `json:"page_size"`
	Offset           int64  `json:"offset"`
}

func (s *QuerySnapshotService) encodeCursor(owner string, payload snapshotCursorPayload) (string, error) {
	payload.Version = 1
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encoding query snapshot cursor: %w", err)
	}
	signature := s.cursorMAC(owner, raw)
	encoded := base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(signature)
	if len(encoded) > maxSnapshotCursorBytes {
		return "", fmt.Errorf("%w: encoded cursor is too large", ErrSnapshotCursor)
	}
	return encoded, nil
}

func (s *QuerySnapshotService) decodeCursor(owner, encoded string) (snapshotCursorPayload, error) {
	if len(encoded) == 0 || len(encoded) > maxSnapshotCursorBytes {
		return snapshotCursorPayload{}, ErrSnapshotCursor
	}
	payloadPart, signaturePart, ok := strings.Cut(encoded, ".")
	if !ok || payloadPart == "" || signaturePart == "" || strings.Contains(signaturePart, ".") {
		return snapshotCursorPayload{}, ErrSnapshotCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		return snapshotCursorPayload{}, ErrSnapshotCursor
	}
	signature, err := base64.RawURLEncoding.DecodeString(signaturePart)
	if err != nil || !hmac.Equal(signature, s.cursorMAC(owner, raw)) {
		return snapshotCursorPayload{}, ErrSnapshotCursor
	}
	var payload snapshotCursorPayload
	if err := json.Unmarshal(raw, &payload, json.RejectUnknownMembers(true)); err != nil {
		return snapshotCursorPayload{}, ErrSnapshotCursor
	}
	if payload.Version != 1 || (payload.PageDirection != "next" && payload.PageDirection != "prev") {
		return snapshotCursorPayload{}, ErrSnapshotCursor
	}
	return payload, nil
}

func (s *QuerySnapshotService) cursorMAC(owner string, payload []byte) []byte {
	digest := hmac.New(sha256.New, s.hmacKey)
	_, _ = digest.Write([]byte(owner))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(payload)
	return digest.Sum(nil)
}
