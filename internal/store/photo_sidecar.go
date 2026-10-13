package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"

	"go.kenn.io/docbank/document"
)

const photoOriginalPathSQL = `(SELECT p.original_path FROM provenance p JOIN ingests i ON i.id=p.ingest_id
 WHERE p.node_id=n.id AND i.source_kind NOT LIKE 'embedded:%' AND NOT EXISTS (SELECT 1 FROM provenance successor WHERE successor.supersedes=p.identity)
 ORDER BY p.identity LIMIT 1)`

type PhotoSidecarTarget struct {
	AssetID              string
	AssetRevision        int64
	MemberRevisions      string
	SidecarFileID        string
	NodeID               int64
	VersionID            string
	SourceSHA256         string
	Size                 int64
	ExtractorFingerprint string
}

// photoSidecarMemberRevisionsSQL snapshots each member's node revision and
// audit coverage. Enrolling a scope changes coverage without a revision, and an
// audited vault only accepts decisions for covered originals.
const photoSidecarMemberRevisionsSQL = `(SELECT group_concat(member_revision, ',') FROM
 (SELECT n.id || ':' || n.revision ||
 CASE WHEN EXISTS (SELECT 1 FROM audit_memberships m WHERE m.node_id=n.id) THEN ':audited' ELSE '' END AS member_revision
 FROM photo_files f JOIN nodes n ON n.id=f.node_id
 WHERE f.asset_id=a.asset_id ORDER BY n.id))`

// photoSidecarConsideredSQL matches a cached outcome for the sidecar row
// aliased file, whose node is aliased node, at the photo's current revision,
// member snapshot, content version, and running extractor.
func photoSidecarConsideredSQL(file, node, alias string) string {
	return `EXISTS (SELECT 1 FROM photo_sidecar_considered ` + alias + ` WHERE ` + alias + `.sidecar_file_id=` + file + `.file_id
 AND ` + alias + `.asset_id=` + file + `.asset_id AND ` + alias + `.asset_revision=a.revision
 AND ` + alias + `.member_revisions=` + photoSidecarMemberRevisionsSQL + `
 AND ` + alias + `.version_id=` + node + `.current_version_id AND ` + alias + `.extractor_fingerprint=g.extractor_fingerprint)`
}

// photoSidecarCandidatesSQL lists unsettled sidecars with current packets in
// node-ID order. A sidecar also waits until every live lower-ID sibling in the
// same photo has settled for the current photo state. Neither extraction order
// nor a listing cursor can then let a higher-ID packet claim an original first.
// The asset revision and member snapshot fence every change to that sibling
// set between listing and initialization.
var photoSidecarCandidatesSQL = `SELECT sidecar.asset_id,a.revision,` + photoSidecarMemberRevisionsSQL + `,sidecar.file_id,n.id,n.current_version_id,v.blob_hash,v.size
 FROM photo_files sidecar JOIN photo_assets a ON a.asset_id=sidecar.asset_id
 JOIN nodes n ON n.id=sidecar.node_id
 JOIN content_versions v ON v.version_id=n.current_version_id
 JOIN source_metadata_generations g ON g.source_sha256=v.blob_hash AND g.contract_version=? AND g.extractor_fingerprint=?
 WHERE sidecar.role='sidecar' AND sidecar.sidecar_of_file_id IS NOT NULL AND n.trashed_at IS NULL
 AND NOT EXISTS (SELECT 1 FROM photo_files sib JOIN nodes sn ON sn.id=sib.node_id
 WHERE sib.asset_id=sidecar.asset_id AND sib.role='sidecar' AND sib.sidecar_of_file_id IS NOT NULL
 AND sib.node_id<sidecar.node_id AND sn.trashed_at IS NULL AND NOT ` + photoSidecarConsideredSQL("sib", "sn", "sc") + `)
 AND NOT ` + photoSidecarConsideredSQL("sidecar", "n", "c") + `
 AND sidecar.node_id>? ORDER BY sidecar.node_id LIMIT ?`

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
		if err := rows.Scan(&t.AssetID, &t.AssetRevision, &t.MemberRevisions, &t.SidecarFileID, &t.NodeID, &t.VersionID, &t.SourceSHA256, &t.Size); err != nil {
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
		sidecar, err := photoFileByIDQuery(ctx, tx, target.SidecarFileID)
		if err != nil {
			return err
		}
		node, err := nodeByIDQuery(ctx, tx, target.NodeID)
		if err != nil {
			return err
		}
		var revision int64
		var members string
		if err := tx.QueryRowContext(ctx, `SELECT a.revision,`+photoSidecarMemberRevisionsSQL+` FROM photo_assets a WHERE a.asset_id=?`, target.AssetID).Scan(&revision, &members); err != nil {
			return err
		}
		if revision != target.AssetRevision || members != target.MemberRevisions || sidecar.AssetID != target.AssetID || sidecar.NodeID != target.NodeID || sidecar.Role != PhotoRoleSidecar || sidecar.SidecarOfID == nil || node.CurrentVersionID != target.VersionID || node.BlobHash != target.SourceSHA256 || node.Size != target.Size || node.TrashedAt != nil {
			return ErrStaleRevision
		}
		metadata, err := photoSidecarEvidence(ctx, tx, target)
		if err != nil {
			return err
		}
		intended, err := photoSidecarIntendedTarget(ctx, tx, sidecar, node, metadata)
		if err != nil {
			return err
		}
		if intended != "" {
			f, err := photoFileByIDQuery(ctx, tx, intended)
			if err != nil {
				return err
			}
			source, err := nodeByIDQuery(ctx, tx, f.NodeID)
			if err != nil {
				return err
			}
			refused, err := auditRefusesAttachmentChangeTx(ctx, tx, f.NodeID)
			if err != nil {
				return err
			}
			if source.TrashedAt == nil && f.Revision == 1 && !refused {
				values, valid, err := photoSidecarValues(metadata)
				if err != nil {
					return err
				}
				patch := PhotoAuthoredPatch{}
				if valid {
					for _, field := range photoAuthoredFields {
						if field.nonDefault(&values) {
							field.setPatch(&patch, &values)
						}
					}
				}
				if patch != (PhotoAuthoredPatch{}) {
					result, err = s.applyPhotoAuthoredTx(ctx, tx, []PhotoAuthoredTarget{{FileID: f.ID, Revision: 1, Patch: patch}}, photoAuthoredApply{sidecar: &PhotoSidecarProvenance{NodeID: target.NodeID, VersionID: target.VersionID, FileID: target.SidecarFileID}})
					if err != nil {
						return err
					}
				}
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO photo_sidecar_considered(sidecar_file_id,asset_id,asset_revision,member_revisions,version_id,extractor_fingerprint)
 SELECT ?,a.asset_id,a.revision,`+photoSidecarMemberRevisionsSQL+`,?,? FROM photo_assets a WHERE a.asset_id=?
 ON CONFLICT(sidecar_file_id) DO UPDATE SET asset_id=excluded.asset_id,asset_revision=excluded.asset_revision,member_revisions=excluded.member_revisions,version_id=excluded.version_id,extractor_fingerprint=excluded.extractor_fingerprint`, target.SidecarFileID, target.VersionID, target.ExtractorFingerprint, target.AssetID)
		return err
	})
	return result, err
}

// auditRefusesAttachmentChangeTx reports whether an audited vault refuses
// attachment changes to the node because no audit scope covers it. Such a
// packet settles without a decision instead of retrying forever.
func auditRefusesAttachmentChangeTx(ctx context.Context, tx *sql.Tx, nodeID int64) (bool, error) {
	var refused bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM audit_authority WHERE singleton=1)
 AND NOT EXISTS(SELECT 1 FROM audit_memberships WHERE node_id=?)`, nodeID).Scan(&refused)
	if err != nil {
		return false, fmt.Errorf("checking audit coverage for photo node %d: %w", nodeID, err)
	}
	return refused, nil
}

func photoSidecarEvidence(ctx context.Context, q metadataQuerier, target PhotoSidecarTarget) (document.SourceMetadataV1, error) {
	var canonical []byte
	var checksum string
	err := q.QueryRowContext(ctx, `SELECT canonical_json,checksum FROM source_metadata_generations WHERE source_sha256=? AND contract_version=? AND extractor_fingerprint=?`, target.SourceSHA256, document.SourceMetadataContractV1, target.ExtractorFingerprint).Scan(&canonical, &checksum)
	if errors.Is(err, sql.ErrNoRows) {
		return document.SourceMetadataV1{}, ErrStaleRevision
	}
	if err != nil {
		return document.SourceMetadataV1{}, err
	}
	metadata, actual, err := document.DecodeSourceMetadataV1(canonical)
	if err != nil || actual != checksum {
		return metadata, ErrSourceMetadataCorrupt
	}
	return metadata, nil
}

func photoSidecarIntendedTarget(ctx context.Context, q metadataQuerier, sidecar PhotoFile, node Node, metadata document.SourceMetadataV1) (string, error) {
	if sidecar.Role != PhotoRoleSidecar || sidecar.SidecarOfID == nil {
		return "", ErrStaleRevision
	}
	extension := ""
	for _, field := range metadata.Fields {
		if field.Namespace == "image.xmp" && field.Key == "image.xmp.sidecar_for_extension" {
			if field.Value.String == nil {
				return "", ErrSourceMetadataCorrupt
			}
			extension = *field.Value.String
		}
	}
	path := node.Name
	err := q.QueryRowContext(ctx, `SELECT COALESCE(`+photoOriginalPathSQL+`,n.name) FROM nodes n WHERE n.id=?`, node.ID).Scan(&path)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	intended, bound, err := photoSidecarSourceTx(ctx, q, sidecar.AssetID, path, extension)
	if err != nil {
		return "", err
	}
	if !bound {
		return *sidecar.SidecarOfID, nil
	}
	if intended == "" {
		return "", nil
	}
	if intended != *sidecar.SidecarOfID {
		var operation string
		var changed bool
		err := q.QueryRowContext(ctx, `SELECT operation,EXISTS
		 (SELECT 1 FROM json_each(r.after_json,'$.member_changes') c WHERE json_extract(c.value,'$.file_id')=?) AS changed
		FROM photo_change_receipts r WHERE asset_id=? AND (json_extract(r.after_json,'$.changes_truncated')=1 OR changed)
		ORDER BY after_revision DESC LIMIT 1`, sidecar.ID, sidecar.AssetID).Scan(&operation, &changed)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		if operation == "import" && changed {
			return intended, nil
		}
	}
	if intended != *sidecar.SidecarOfID {
		return "", nil
	}
	return intended, nil
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
