package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"

	"go.kenn.io/docbank/internal/canonical"
)

const MaxProductionMapChunkBytes = 64 << 10

// ProductionMapChunk is one bounded slice of the retained canonical aligned
// text map. Reassemble Data in Offset order and verify MapSHA256 before use.
type ProductionMapChunk struct {
	MapSHA256   string `json:"map_sha256"`
	Offset      int64  `json:"offset"`
	TotalBytes  int64  `json:"total_bytes"`
	Data        string `json:"data"`
	ChunkSHA256 string `json:"chunk_sha256"`
	NextCursor  string `json:"next_cursor"`
}

type productionMapCursorV1 struct {
	Kind      string `json:"kind"`
	SetID     string `json:"set_id"`
	Revision  int64  `json:"revision"`
	MemberID  string `json:"member_id"`
	MapSHA256 string `json:"map_sha256"`
	Offset    int64  `json:"offset"`
}

func (s *Store) ProductionMapChunk(ctx context.Context, setID string, revision int64, memberID, cursor string, limit int) (ProductionMapChunk, error) {
	if validateUUIDv4(setID) != nil || validateUUIDv4(memberID) != nil || revision < 1 ||
		limit < 1 || limit > MaxProductionMapChunkBytes || len(cursor) > 512 {
		return ProductionMapChunk{}, ErrInvalidProduction
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ProductionMapChunk{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := scanProductionDraft(tx.QueryRowContext(ctx, productionDraftSelect+` WHERE set_id=? AND revision=?`, setID, revision)); err != nil {
		return ProductionMapChunk{}, err
	}
	var mapSHA256 string
	if err := tx.QueryRowContext(ctx, `SELECT map_sha256 FROM production_members WHERE set_id=? AND revision=? AND member_id=?`,
		setID, revision, memberID).Scan(&mapSHA256); errors.Is(err, sql.ErrNoRows) {
		return ProductionMapChunk{}, ErrNotFound
	} else if err != nil {
		return ProductionMapChunk{}, err
	}
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT canonical_json FROM production_text_maps WHERE map_sha256=?`, mapSHA256).Scan(&raw); errors.Is(err, sql.ErrNoRows) {
		return ProductionMapChunk{}, ErrInvalidProduction
	} else if err != nil {
		return ProductionMapChunk{}, err
	}
	mapDigest := sha256.Sum256(raw)
	if hex.EncodeToString(mapDigest[:]) != mapSHA256 || len(raw) == 0 {
		return ProductionMapChunk{}, ErrInvalidProduction
	}
	offset := int64(0)
	if cursor != "" {
		decoded, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
		if err != nil {
			return ProductionMapChunk{}, ErrInvalidProduction
		}
		value, err := canonical.Decode[productionMapCursorV1](decoded)
		if err != nil || value.Kind != "map" || value.SetID != setID || value.Revision != revision ||
			value.MemberID != memberID || value.MapSHA256 != mapSHA256 || value.Offset <= 0 || value.Offset >= int64(len(raw)) {
			return ProductionMapChunk{}, ErrInvalidProduction
		}
		offset = value.Offset
	}
	end := min(offset+int64(limit), int64(len(raw)))
	chunk := raw[offset:end]
	chunkDigest := sha256.Sum256(chunk)
	result := ProductionMapChunk{MapSHA256: mapSHA256, Offset: offset, TotalBytes: int64(len(raw)),
		Data: base64.StdEncoding.EncodeToString(chunk), ChunkSHA256: hex.EncodeToString(chunkDigest[:])}
	if end < int64(len(raw)) {
		next, err := canonical.Marshal(productionMapCursorV1{Kind: "map", SetID: setID, Revision: revision,
			MemberID: memberID, MapSHA256: mapSHA256, Offset: end})
		if err != nil {
			return ProductionMapChunk{}, err
		}
		result.NextCursor = base64.RawURLEncoding.EncodeToString(next)
	}
	if err := tx.Commit(); err != nil {
		return ProductionMapChunk{}, err
	}
	return result, nil
}
