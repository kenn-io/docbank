package store

import (
	"context"
	"database/sql"
	"errors"
	"go.kenn.io/docbank/document"
	"math"
	"strconv"
)

type PhotoSidecarTarget struct {
	FileID               string
	AssetID              string
	SidecarFileID        string
	NodeID               int64
	VersionID            string
	SourceSHA256         string
	Size                 int64
	ExtractorFingerprint string
}

const photoSidecarCandidatesSQL = `SELECT target.file_id,target.asset_id,sidecar.file_id,n.id,n.current_version_id,v.blob_hash,v.size
 FROM photo_files sidecar JOIN photo_files target ON target.file_id=sidecar.sidecar_of_file_id
 JOIN nodes n ON n.id=sidecar.node_id JOIN nodes source ON source.id=target.node_id
 JOIN content_versions v ON v.version_id=n.current_version_id
 JOIN source_metadata_generations g ON g.source_sha256=v.blob_hash AND g.contract_version=? AND g.extractor_fingerprint=?
 WHERE sidecar.role='sidecar' AND target.revision=1 AND sidecar.asset_id=target.asset_id AND n.trashed_at IS NULL AND source.trashed_at IS NULL
 AND NOT EXISTS (SELECT 1 FROM photo_sidecar_considered c WHERE c.sidecar_file_id=sidecar.file_id AND c.target_file_id=target.file_id AND c.version_id=n.current_version_id AND c.extractor_fingerprint=g.extractor_fingerprint)
 AND n.id>? ORDER BY n.id LIMIT ?`

func (s *Store) MissingPhotoSidecarsAfter(ctx context.Context, fingerprint, after string, limit int) ([]PhotoSidecarTarget, error) {
	if limit < 1 || limit > maxBatchTagTargets || validateCatalogSHA256(fingerprint, "extractor fingerprint") != nil {
		return nil, ErrInvalidPhotoAsset
	}
	var cursor int64
	if after != "" {
		var err error
		cursor, err = strconv.ParseInt(after, 10, 64)
		if err != nil || cursor < 1 {
			return nil, ErrInvalidPhotoAsset
		}
	}
	rows, err := s.db.QueryContext(ctx, photoSidecarCandidatesSQL, document.SourceMetadataContractV1, fingerprint, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	targets := []PhotoSidecarTarget{}
	for rows.Next() {
		var t PhotoSidecarTarget
		if err := rows.Scan(&t.FileID, &t.AssetID, &t.SidecarFileID, &t.NodeID, &t.VersionID, &t.SourceSHA256, &t.Size); err != nil {
			return nil, err
		}
		t.ExtractorFingerprint = fingerprint
		targets = append(targets, t)
	}
	return targets, rows.Err()
}

func (s *Store) InitializePhotoSidecar(ctx context.Context, target PhotoSidecarTarget) (PhotoAuthoredReceipt, error) {
	var result PhotoAuthoredReceipt
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		f, err := photoFileByIDQuery(ctx, tx, target.FileID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if f.Revision != 1 {
			return nil
		}
		sidecar, err := photoFileByIDQuery(ctx, tx, target.SidecarFileID)
		if err != nil {
			return err
		}
		node, err := nodeByIDQuery(ctx, tx, target.NodeID)
		if err != nil {
			return err
		}
		source, err := nodeByIDQuery(ctx, tx, f.NodeID)
		if err != nil {
			return err
		}
		if source.TrashedAt != nil {
			return ErrStaleRevision
		}
		if f.AssetID != target.AssetID || sidecar.AssetID != target.AssetID || sidecar.NodeID != target.NodeID || sidecar.Role != PhotoRoleSidecar || sidecar.SidecarOfID == nil || *sidecar.SidecarOfID != f.ID || node.CurrentVersionID != target.VersionID || node.BlobHash != target.SourceSHA256 || node.Size != target.Size || node.TrashedAt != nil {
			return ErrStaleRevision
		}
		var canonical []byte
		var checksum string
		err = tx.QueryRowContext(ctx, `SELECT canonical_json,checksum FROM source_metadata_generations WHERE source_sha256=? AND contract_version=? AND extractor_fingerprint=?`, target.SourceSHA256, document.SourceMetadataContractV1, target.ExtractorFingerprint).Scan(&canonical, &checksum)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrStaleRevision
		}
		if err != nil {
			return err
		}
		metadata, actual, err := document.DecodeSourceMetadataV1(canonical)
		if err != nil || actual != checksum {
			return ErrSourceMetadataCorrupt
		}
		values, valid, err := photoSidecarValues(metadata)
		if err != nil {
			return err
		}
		if !valid || values == (PhotoAuthored{}) {
			_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO photo_sidecar_considered(sidecar_file_id,target_file_id,version_id,extractor_fingerprint) VALUES(?,?,?,?)`, target.SidecarFileID, target.FileID, target.VersionID, target.ExtractorFingerprint)
			return err
		}
		v := values
		patch := PhotoAuthoredPatch{&v.Rating, &v.Flag, &v.Label, &v.Caption, &v.Creator, &v.Copyright, &v.Rotation}
		result, err = s.applyPhotoAuthoredTx(ctx, tx, []PhotoAuthoredTarget{{FileID: f.ID, Revision: 1, Patch: patch}}, "", &PhotoSidecarProvenance{NodeID: target.NodeID, VersionID: target.VersionID, FileID: target.SidecarFileID}, false)
		return err
	})
	return result, err
}

func photoSidecarValues(metadata document.SourceMetadataV1) (PhotoAuthored, bool, error) {
	var values PhotoAuthored
	valid := false
	for _, field := range metadata.Fields {
		if field.Namespace != "image.xmp" {
			continue
		}
		switch field.Key {
		case "image.xmp.packet_valid":
			if field.Value.Boolean == nil {
				return values, false, ErrSourceMetadataCorrupt
			}
			valid = *field.Value.Boolean
		case "image.xmp.rating", "image.xmp.rotation":
			if field.Value.Integer == nil || *field.Value.Integer < math.MinInt || *field.Value.Integer > math.MaxInt {
				return values, false, ErrSourceMetadataCorrupt
			}
			n := int(*field.Value.Integer)
			if field.Key == "image.xmp.rating" {
				values.Rating = n
			} else {
				values.Rotation = n
			}
		case "image.xmp.flag", "image.xmp.label", "image.xmp.caption", "image.xmp.creator", "image.xmp.copyright":
			if field.Value.String == nil {
				return values, false, ErrSourceMetadataCorrupt
			}
			switch field.Key {
			case "image.xmp.flag":
				values.Flag = *field.Value.String
			case "image.xmp.label":
				values.Label = *field.Value.String
			case "image.xmp.caption":
				values.Caption = *field.Value.String
			case "image.xmp.creator":
				values.Creator = *field.Value.String
			case "image.xmp.copyright":
				values.Copyright = *field.Value.String
			}
		}
	}
	return values, valid, nil
}
