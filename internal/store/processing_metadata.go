package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"unicode/utf8"

	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/document"
)

const (
	metadataProcessingProfileType = "processing_profile"
	metadataRenditionBuildType    = "rendition_build"
	metadataRenditionArtifactType = "rendition_artifact"
	metadataRenditionUnitType     = "rendition_unit"
	metadataRenditionSegmentType  = "rendition_lexical_segment"
	metadataRenditionAttachType   = "rendition_attachment"
	metadataRenditionHeadType     = "rendition_head"
)

type metadataProcessingProfile struct {
	Type                           string         `json:"type"`
	Fingerprint                    string         `json:"profile_fingerprint"`
	CanonicalProfile               jsontext.Value `json:"canonical_profile"`
	RenditionRequestFingerprint    string         `json:"rendition_request_fingerprint"`
	EvidenceLexicalFingerprint     string         `json:"evidence_lexical_fingerprint"`
	RetentionDisclosureFingerprint string         `json:"retention_disclosure_fingerprint"`
	AttachmentPolicyFingerprint    string         `json:"attachment_policy_fingerprint"`
	ConsentFingerprint             string         `json:"consent_fingerprint"`
	RenditionDisclosureFingerprint string         `json:"rendition_disclosure_fingerprint"`
	TrustBoundary                  string         `json:"trust_boundary"`
}

type metadataRenditionBuild struct {
	Type                              string                        `json:"type"`
	ID                                string                        `json:"build_id"`
	VaultID                           string                        `json:"vault_id"`
	SourceSHA256                      string                        `json:"source_sha256"`
	RenditionRequestFingerprint       string                        `json:"rendition_request_fingerprint"`
	EvidenceLexicalFingerprint        string                        `json:"evidence_lexical_fingerprint"`
	CapturedArtifactPolicyFingerprint string                        `json:"captured_artifact_policy_fingerprint"`
	CapturedArtifactPolicy            jsontext.Value                `json:"captured_artifact_policy"`
	AuthorizationChecksum             string                        `json:"authorization_checksum"`
	ProviderOperationID               string                        `json:"provider_operation_id"`
	ProviderReceipt                   jsontext.Value                `json:"provider_receipt"`
	EvidenceChecksum                  string                        `json:"evidence_checksum"`
	RenditionChecksum                 string                        `json:"rendition_checksum"`
	MarkdownChecksum                  string                        `json:"markdown_checksum"`
	Completeness                      document.EvidenceCompleteness `json:"completeness"`
	PartialSuccess                    bool                          `json:"partial_success"`
	Truncated                         bool                          `json:"truncated"`
	Warnings                          []string                      `json:"warnings"`
	CompletedAt                       string                        `json:"completed_at"`
	DeclaredArtifactCount             int                           `json:"declared_artifact_count"`
	UnitCount                         int                           `json:"unit_count"`
	LexicalSegmentCount               int                           `json:"lexical_segment_count"`
}

type metadataRenditionArtifact struct {
	Type       string                 `json:"type"`
	BuildID    string                 `json:"build_id"`
	ArtifactID string                 `json:"artifact_id"`
	Role       string                 `json:"role"`
	BlobHash   string                 `json:"blob_hash"`
	Size       int64                  `json:"size"`
	Checksum   string                 `json:"checksum"`
	State      RenditionArtifactState `json:"state"`
}

type metadataRenditionUnit struct {
	Type           string                     `json:"type"`
	BuildID        string                     `json:"build_id"`
	UnitID         string                     `json:"unit_id"`
	EvidenceUnitID string                     `json:"evidence_unit_id"`
	Order          int                        `json:"order"`
	Checksum       string                     `json:"checksum"`
	HeadingPath    []string                   `json:"heading_path"`
	Locator        document.EvidenceLocatorV1 `json:"locator"`
}

type metadataRenditionSegment struct {
	Type      string `json:"type"`
	BuildID   string `json:"build_id"`
	SegmentID string `json:"segment_id"`
	UnitID    string `json:"unit_id"`
	Order     int    `json:"order"`
	CharStart int    `json:"char_start"`
	CharEnd   int    `json:"char_end"`
	Checksum  string `json:"checksum"`
	Text      string `json:"text"`
}

type metadataRenditionAttachment struct {
	Type                           string `json:"type"`
	AttachmentID                   string `json:"attachment_id"`
	VaultID                        string `json:"vault_id"`
	ContentVersionID               string `json:"content_version_id"`
	BuildID                        string `json:"build_id"`
	ProcessingProfileFingerprint   string `json:"processing_profile_fingerprint"`
	RetentionDisclosureFingerprint string `json:"retention_disclosure_fingerprint"`
	AttachmentPolicyFingerprint    string `json:"attachment_policy_fingerprint"`
	ConsentFingerprint             string `json:"consent_fingerprint"`
	RenditionDisclosureFingerprint string `json:"rendition_disclosure_fingerprint"`
	TrustBoundary                  string `json:"trust_boundary"`
	AttachedAt                     string `json:"attached_at"`
}

type metadataRenditionHead struct {
	Type                         string `json:"type"`
	ContentVersionID             string `json:"content_version_id"`
	ProcessingProfileFingerprint string `json:"processing_profile_fingerprint"`
	AttachmentID                 string `json:"attachment_id"`
	PublishedAt                  string `json:"published_at"`
}

var processingMetadataRequiredFields = map[string][]string{
	metadataProcessingProfileType: {
		metadataTypeField, "profile_fingerprint", "canonical_profile",
		"rendition_request_fingerprint", "evidence_lexical_fingerprint",
		"retention_disclosure_fingerprint", "attachment_policy_fingerprint",
		"consent_fingerprint", "rendition_disclosure_fingerprint", "trust_boundary",
	},
	metadataRenditionBuildType: {
		metadataTypeField, "build_id", auditVaultIDField, "source_sha256",
		"rendition_request_fingerprint", "evidence_lexical_fingerprint",
		"captured_artifact_policy_fingerprint", "captured_artifact_policy",
		"authorization_checksum", "provider_operation_id", "provider_receipt",
		"evidence_checksum", "rendition_checksum", "markdown_checksum", "completeness",
		"partial_success", "truncated", "warnings", "completed_at",
		"declared_artifact_count", "unit_count", "lexical_segment_count",
	},
	metadataRenditionArtifactType: {
		metadataTypeField, "build_id", "artifact_id", "role", "blob_hash",
		metadataSizeField, "checksum", "state",
	},
	metadataRenditionUnitType: {
		metadataTypeField, "build_id", "unit_id", "evidence_unit_id", "order",
		"checksum", "heading_path", "locator",
	},
	metadataRenditionSegmentType: {
		metadataTypeField, "build_id", "segment_id", "unit_id", "order",
		"char_start", "char_end", "checksum", "text",
	},
	metadataRenditionAttachType: {
		metadataTypeField, "attachment_id", auditVaultIDField, "content_version_id",
		"build_id", "processing_profile_fingerprint",
		"retention_disclosure_fingerprint", "attachment_policy_fingerprint",
		"consent_fingerprint", "rendition_disclosure_fingerprint", "trust_boundary",
		"attached_at",
	},
	metadataRenditionHeadType: {
		metadataTypeField, "content_version_id", "processing_profile_fingerprint",
		"attachment_id", "published_at",
	},
}

func exportProcessingMetadata(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	present, err := processingMetadataSchemaPresent(ctx, tx)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	if err := exportProcessingProfiles(ctx, tx, write); err != nil {
		return err
	}
	if err := exportRenditionBuilds(ctx, tx, write); err != nil {
		return err
	}
	if err := exportRenditionArtifacts(ctx, tx, write); err != nil {
		return err
	}
	if err := exportRenditionUnits(ctx, tx, write); err != nil {
		return err
	}
	if err := exportRenditionSegments(ctx, tx, write); err != nil {
		return err
	}
	if err := exportRenditionAttachments(ctx, tx, write); err != nil {
		return err
	}
	return exportRenditionHeads(ctx, tx, write)
}

func exportProcessingProfiles(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT profile_fingerprint,canonical_profile,rendition_request_fingerprint,
		       evidence_lexical_fingerprint,retention_disclosure_fingerprint,
		       attachment_policy_fingerprint,consent_fingerprint,
		       rendition_disclosure_fingerprint,trust_boundary
		FROM processing_profiles ORDER BY profile_fingerprint`)
	if err != nil {
		return fmt.Errorf("exporting processing profiles: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := metadataProcessingProfile{Type: metadataProcessingProfileType}
		var canonical string
		if err := rows.Scan(&record.Fingerprint, &canonical, &record.RenditionRequestFingerprint,
			&record.EvidenceLexicalFingerprint, &record.RetentionDisclosureFingerprint,
			&record.AttachmentPolicyFingerprint, &record.ConsentFingerprint,
			&record.RenditionDisclosureFingerprint, &record.TrustBoundary); err != nil {
			return fmt.Errorf("scanning processing profile metadata: %w", err)
		}
		record.CanonicalProfile = jsontext.Value(canonical)
		if err := write(record); err != nil {
			return err
		}
	}
	return rowsError("processing profile", rows)
}

func exportRenditionBuilds(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT build_id,vault_uid,source_sha256,rendition_request_fingerprint,
		       evidence_lexical_fingerprint,captured_artifact_policy_fingerprint,
		       captured_artifact_policy_json,authorization_checksum,provider_operation_id,
		       provider_receipt_json,evidence_checksum,rendition_checksum,markdown_checksum,
		       completeness,partial_success,truncated,warnings_json,completed_at,
		       declared_artifact_count,unit_count,lexical_segment_count
		FROM rendition_builds ORDER BY build_id`)
	if err != nil {
		return fmt.Errorf("exporting rendition builds: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := metadataRenditionBuild{Type: metadataRenditionBuildType}
		var policy, receipt, warnings string
		if err := rows.Scan(&record.ID, &record.VaultID, &record.SourceSHA256,
			&record.RenditionRequestFingerprint, &record.EvidenceLexicalFingerprint,
			&record.CapturedArtifactPolicyFingerprint, &policy, &record.AuthorizationChecksum,
			&record.ProviderOperationID, &receipt, &record.EvidenceChecksum,
			&record.RenditionChecksum, &record.MarkdownChecksum, &record.Completeness,
			&record.PartialSuccess, &record.Truncated, &warnings, &record.CompletedAt,
			&record.DeclaredArtifactCount, &record.UnitCount, &record.LexicalSegmentCount); err != nil {
			return fmt.Errorf("scanning rendition build metadata: %w", err)
		}
		record.CapturedArtifactPolicy = jsontext.Value(policy)
		record.ProviderReceipt = jsontext.Value(receipt)
		if err := json.Unmarshal([]byte(warnings), &record.Warnings); err != nil {
			return fmt.Errorf("decoding rendition build warnings: %w", err)
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rowsError("rendition build", rows)
}

func exportRenditionArtifacts(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT build_id,artifact_id,role,blob_hash,size,checksum,state
		FROM rendition_artifacts ORDER BY build_id,artifact_id`)
	if err != nil {
		return fmt.Errorf("exporting rendition artifacts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := metadataRenditionArtifact{Type: metadataRenditionArtifactType}
		if err := rows.Scan(&record.BuildID, &record.ArtifactID, &record.Role,
			&record.BlobHash, &record.Size, &record.Checksum, &record.State); err != nil {
			return fmt.Errorf("scanning rendition artifact metadata: %w", err)
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rowsError("rendition artifact", rows)
}

func exportRenditionUnits(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT build_id,unit_id,evidence_unit_id,unit_order,checksum,heading_path_json,locator_json
		FROM rendition_units ORDER BY build_id,unit_order,unit_id`)
	if err != nil {
		return fmt.Errorf("exporting rendition units: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := metadataRenditionUnit{Type: metadataRenditionUnitType}
		var headingPath, locator string
		if err := rows.Scan(&record.BuildID, &record.UnitID, &record.EvidenceUnitID,
			&record.Order, &record.Checksum, &headingPath, &locator); err != nil {
			return fmt.Errorf("scanning rendition unit metadata: %w", err)
		}
		if err := json.Unmarshal([]byte(headingPath), &record.HeadingPath); err != nil {
			return fmt.Errorf("decoding rendition heading path: %w", err)
		}
		if err := json.Unmarshal([]byte(locator), &record.Locator); err != nil {
			return fmt.Errorf("decoding rendition locator: %w", err)
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rowsError("rendition unit", rows)
}

func exportRenditionSegments(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT build_id,segment_id,unit_id,segment_order,char_start,char_end,checksum,text
		FROM rendition_lexical_segments ORDER BY build_id,segment_order,segment_id`)
	if err != nil {
		return fmt.Errorf("exporting rendition lexical segments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := metadataRenditionSegment{Type: metadataRenditionSegmentType}
		if err := rows.Scan(&record.BuildID, &record.SegmentID, &record.UnitID, &record.Order,
			&record.CharStart, &record.CharEnd, &record.Checksum, &record.Text); err != nil {
			return fmt.Errorf("scanning rendition lexical segment metadata: %w", err)
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rowsError("rendition lexical segment", rows)
}

func exportRenditionAttachments(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT attachment_id,vault_uid,content_version_id,build_id,profile_fingerprint,
		       retention_disclosure_fingerprint,attachment_policy_fingerprint,
		       consent_fingerprint,rendition_disclosure_fingerprint,trust_boundary,attached_at
		FROM rendition_attachments
		ORDER BY content_version_id,profile_fingerprint,attachment_id`)
	if err != nil {
		return fmt.Errorf("exporting rendition attachments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := metadataRenditionAttachment{Type: metadataRenditionAttachType}
		if err := rows.Scan(&record.AttachmentID, &record.VaultID, &record.ContentVersionID,
			&record.BuildID, &record.ProcessingProfileFingerprint,
			&record.RetentionDisclosureFingerprint, &record.AttachmentPolicyFingerprint,
			&record.ConsentFingerprint, &record.RenditionDisclosureFingerprint,
			&record.TrustBoundary, &record.AttachedAt); err != nil {
			return fmt.Errorf("scanning rendition attachment metadata: %w", err)
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rowsError("rendition attachment", rows)
}

func exportRenditionHeads(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT content_version_id,profile_fingerprint,attachment_id,published_at
		FROM rendition_heads ORDER BY content_version_id,profile_fingerprint`)
	if err != nil {
		return fmt.Errorf("exporting rendition heads: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := metadataRenditionHead{Type: metadataRenditionHeadType}
		if err := rows.Scan(&record.ContentVersionID, &record.ProcessingProfileFingerprint,
			&record.AttachmentID, &record.PublishedAt); err != nil {
			return fmt.Errorf("scanning rendition head metadata: %w", err)
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rowsError("rendition head", rows)
}

func isProcessingMetadataType(kind string) bool {
	_, ok := processingMetadataRequiredFields[kind]
	return ok
}

func (s *Store) importProcessingMetadataRecord(
	ctx context.Context, tx *sql.Tx, kind string, raw jsontext.Value, verifyPhysicalBytes bool,
) error {
	switch kind {
	case metadataProcessingProfileType:
		var value metadataProcessingProfile
		if err := decodeMetadataRecord(raw, &value); err != nil {
			return err
		}
		record, err := normalizeProcessingProfileRecord(ProcessingProfileRecord{
			Fingerprint: value.Fingerprint, CanonicalProfile: value.CanonicalProfile,
			RenditionRequestFingerprint:    value.RenditionRequestFingerprint,
			EvidenceLexicalFingerprint:     value.EvidenceLexicalFingerprint,
			RetentionDisclosureFingerprint: value.RetentionDisclosureFingerprint,
			AttachmentPolicyFingerprint:    value.AttachmentPolicyFingerprint,
			ConsentFingerprint:             value.ConsentFingerprint,
			RenditionDisclosureFingerprint: value.RenditionDisclosureFingerprint,
			TrustBoundary:                  value.TrustBoundary,
		})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO processing_profiles(
				profile_fingerprint,canonical_profile,rendition_request_fingerprint,
				evidence_lexical_fingerprint,retention_disclosure_fingerprint,
				attachment_policy_fingerprint,consent_fingerprint,
				rendition_disclosure_fingerprint,trust_boundary
			) VALUES(?,?,?,?,?,?,?,?,?)`, record.Fingerprint, string(record.CanonicalProfile),
			record.RenditionRequestFingerprint, record.EvidenceLexicalFingerprint,
			record.RetentionDisclosureFingerprint, record.AttachmentPolicyFingerprint,
			record.ConsentFingerprint, record.RenditionDisclosureFingerprint, record.TrustBoundary)
		return err
	case metadataRenditionBuildType:
		var value metadataRenditionBuild
		if err := decodeMetadataRecord(raw, &value); err != nil {
			return err
		}
		if err := validateMetadataRenditionBuild(value); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_builds(
				build_id,vault_uid,source_sha256,rendition_request_fingerprint,
				evidence_lexical_fingerprint,captured_artifact_policy_fingerprint,
				captured_artifact_policy_json,authorization_checksum,provider_operation_id,
				provider_receipt_json,evidence_checksum,rendition_checksum,markdown_checksum,
				completeness,partial_success,truncated,warnings_json,completed_at,
				declared_artifact_count,unit_count,lexical_segment_count
			) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, value.ID, value.VaultID,
			value.SourceSHA256, value.RenditionRequestFingerprint, value.EvidenceLexicalFingerprint,
			value.CapturedArtifactPolicyFingerprint, string(value.CapturedArtifactPolicy),
			value.AuthorizationChecksum, value.ProviderOperationID, string(value.ProviderReceipt),
			value.EvidenceChecksum, value.RenditionChecksum, value.MarkdownChecksum,
			value.Completeness, value.PartialSuccess, value.Truncated, mustCatalogJSON(value.Warnings),
			value.CompletedAt, value.DeclaredArtifactCount, value.UnitCount, value.LexicalSegmentCount)
		return err
	case metadataRenditionArtifactType:
		var value metadataRenditionArtifact
		if err := decodeMetadataRecord(raw, &value); err != nil {
			return err
		}
		if err := validateMetadataRenditionArtifact(value); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_artifacts(build_id,artifact_id,role,blob_hash,size,checksum,state)
			VALUES(?,?,?,?,?,?,?)`, value.BuildID, value.ArtifactID, value.Role,
			value.BlobHash, value.Size, value.Checksum, value.State)
		return err
	case metadataRenditionUnitType:
		var value metadataRenditionUnit
		if err := decodeMetadataRecord(raw, &value); err != nil {
			return err
		}
		if err := validateMetadataRenditionUnit(value); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_units(
				build_id,unit_id,evidence_unit_id,unit_order,checksum,heading_path_json,locator_json
			) VALUES(?,?,?,?,?,?,?)`, value.BuildID, value.UnitID, value.EvidenceUnitID,
			value.Order, value.Checksum, mustCatalogJSON(value.HeadingPath), mustCatalogJSON(value.Locator))
		return err
	case metadataRenditionSegmentType:
		var value metadataRenditionSegment
		if err := decodeMetadataRecord(raw, &value); err != nil {
			return err
		}
		if err := validateMetadataRenditionSegment(value); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_lexical_segments(
				build_id,segment_id,unit_id,segment_order,char_start,char_end,checksum,text
			) VALUES(?,?,?,?,?,?,?,?)`, value.BuildID, value.SegmentID, value.UnitID,
			value.Order, value.CharStart, value.CharEnd, value.Checksum, value.Text)
		return err
	case metadataRenditionAttachType:
		var value metadataRenditionAttachment
		if err := decodeMetadataRecord(raw, &value); err != nil {
			return err
		}
		if err := validateMetadataRenditionAttachment(value); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_attachments(
				attachment_id,vault_uid,content_version_id,build_id,profile_fingerprint,
				retention_disclosure_fingerprint,attachment_policy_fingerprint,
				consent_fingerprint,rendition_disclosure_fingerprint,trust_boundary,attached_at
			) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, value.AttachmentID, value.VaultID,
			value.ContentVersionID, value.BuildID, value.ProcessingProfileFingerprint,
			value.RetentionDisclosureFingerprint, value.AttachmentPolicyFingerprint,
			value.ConsentFingerprint, value.RenditionDisclosureFingerprint,
			value.TrustBoundary, value.AttachedAt)
		return err
	case metadataRenditionHeadType:
		var value metadataRenditionHead
		if err := decodeMetadataRecord(raw, &value); err != nil {
			return err
		}
		if err := validateRenditionHeadRecord(RenditionHeadRecord{
			ContentVersionID:             value.ContentVersionID,
			ProcessingProfileFingerprint: value.ProcessingProfileFingerprint,
			AttachmentID:                 value.AttachmentID, PublishedAt: value.PublishedAt,
		}); err != nil {
			return err
		}
		if err := s.validateImportedRenditionHead(ctx, tx, value, verifyPhysicalBytes); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_heads(content_version_id,profile_fingerprint,attachment_id,published_at)
			VALUES(?,?,?,?)`, value.ContentVersionID, value.ProcessingProfileFingerprint,
			value.AttachmentID, value.PublishedAt)
		return err
	default:
		return fmt.Errorf("unknown processing metadata type %q", kind)
	}
}

type importedProcessingBlob struct {
	hash string
	size int64
}

// RenditionBlobReader opens catalog-authorized loose or packed content for
// post-restore verification.
type RenditionBlobReader interface {
	OpenStreamContext(ctx context.Context, hash string) (packstore.VerifiedReadCloser, int64, error)
}

// VerifyRenditionHeadBytes verifies every source and artifact reachable from
// an active rendition head through the restored mixed-storage catalog.
func (s *Store) VerifyRenditionHeadBytes(ctx context.Context, reader RenditionBlobReader) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.source_sha256, source.size
		FROM rendition_heads h
		JOIN rendition_attachments a
		  ON a.content_version_id=h.content_version_id
		 AND a.profile_fingerprint=h.profile_fingerprint
		 AND a.attachment_id=h.attachment_id
		JOIN rendition_builds b ON b.build_id=a.build_id AND b.vault_uid=a.vault_uid
		JOIN blobs source ON source.hash=b.source_sha256
		UNION
		SELECT artifact.blob_hash, artifact.size
		FROM rendition_heads h
		JOIN rendition_attachments a
		  ON a.content_version_id=h.content_version_id
		 AND a.profile_fingerprint=h.profile_fingerprint
		 AND a.attachment_id=h.attachment_id
		JOIN rendition_artifacts artifact ON artifact.build_id=a.build_id
		ORDER BY 1, 2`)
	if err != nil {
		return fmt.Errorf("listing active rendition bytes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var blob importedProcessingBlob
		if err := rows.Scan(&blob.hash, &blob.size); err != nil {
			return fmt.Errorf("scanning active rendition bytes: %w", err)
		}
		if err := verifyRenditionBlob(ctx, reader, blob); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating active rendition bytes: %w", err)
	}
	return nil
}

func verifyRenditionBlob(
	ctx context.Context, reader RenditionBlobReader, blob importedProcessingBlob,
) (retErr error) {
	stream, logicalSize, err := reader.OpenStreamContext(ctx, blob.hash)
	if err != nil {
		return fmt.Errorf("opening restored rendition blob %s: %w", blob.hash, err)
	}
	defer func() { retErr = errors.Join(retErr, stream.Close()) }()
	if logicalSize != blob.size {
		return fmt.Errorf(
			"restored rendition blob %s size %d does not match catalog size %d",
			blob.hash, logicalSize, blob.size,
		)
	}
	read, err := io.Copy(io.Discard, stream)
	if err != nil {
		return fmt.Errorf("reading restored rendition blob %s: %w", blob.hash, err)
	}
	if read != blob.size {
		return fmt.Errorf(
			"restored rendition blob %s read %d bytes, want %d", blob.hash, read, blob.size,
		)
	}
	if err := stream.Verify(); err != nil {
		return fmt.Errorf("verifying restored rendition blob %s: %w", blob.hash, err)
	}
	return nil
}

func (s *Store) validateImportedRenditionHead(
	ctx context.Context, tx *sql.Tx, head metadataRenditionHead, verifyPhysicalBytes bool,
) (retErr error) {
	var buildID string
	var source importedProcessingBlob
	err := tx.QueryRowContext(ctx, `
		SELECT a.build_id,b.source_sha256,source.size
		FROM rendition_attachments a
		JOIN rendition_builds b ON b.build_id=a.build_id AND b.vault_uid=a.vault_uid
		JOIN blobs source ON source.hash=b.source_sha256
		WHERE a.attachment_id=? AND a.content_version_id=? AND a.profile_fingerprint=?`,
		head.AttachmentID, head.ContentVersionID, head.ProcessingProfileFingerprint,
	).Scan(&buildID, &source.hash, &source.size)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("processing head cannot verify bytes without its exact attachment and build")
	}
	if err != nil {
		return fmt.Errorf("reading processing head byte authority: %w", err)
	}
	if err := validateRenditionBuildStateTx(ctx, tx, buildID); err != nil {
		return err
	}
	blobs := []importedProcessingBlob{source}
	rows, err := tx.QueryContext(ctx, `
		SELECT blob_hash,size FROM rendition_artifacts
		WHERE build_id=? ORDER BY artifact_id`, buildID)
	if err != nil {
		return fmt.Errorf("reading imported rendition artifact bytes: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("closing imported rendition artifact bytes: %w", err))
		}
	}()
	for rows.Next() {
		var blob importedProcessingBlob
		if err := rows.Scan(&blob.hash, &blob.size); err != nil {
			return fmt.Errorf("scanning imported rendition artifact bytes: %w", err)
		}
		blobs = append(blobs, blob)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating imported rendition artifact bytes: %w", err)
	}
	if !verifyPhysicalBytes {
		return nil
	}

	layout, err := packstore.NewLayout(
		filepath.Join(filepath.Dir(s.path), "blobs"),
		packstore.LayoutOptions{Staging: packstore.StagingStoreDirectory, StagingDir: "tmp"},
	)
	if err != nil {
		return fmt.Errorf("opening processing blob layout: %w", err)
	}
	backend, err := packstore.NewFilesystemBackend(layout, packstore.FilesystemBackendOptions{})
	if err != nil {
		return fmt.Errorf("opening processing blob verifier: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, backend.Close()) }()
	verified := make(map[string]bool, len(blobs))
	for _, blob := range blobs {
		if verified[blob.hash] {
			continue
		}
		if err := verifyImportedProcessingBlob(ctx, backend, blob); err != nil {
			return err
		}
		verified[blob.hash] = true
	}
	return nil
}

func verifyImportedProcessingBlob(
	ctx context.Context, backend *packstore.FilesystemBackend, blob importedProcessingBlob,
) (retErr error) {
	hash, err := packstore.ParseHash(blob.hash)
	if err != nil {
		return fmt.Errorf("parsing imported processing blob %s: %w", blob.hash, err)
	}
	stream, logicalSize, err := backend.OpenLoose(ctx, hash, packstore.LooseLocation{
		Encoding: packstore.LooseEncodingRaw, LogicalSize: blob.size, StoredSize: blob.size,
	})
	if err != nil {
		return fmt.Errorf("opening physical processing blob %s: %w", blob.hash, err)
	}
	defer func() { retErr = errors.Join(retErr, stream.Close()) }()
	if logicalSize != blob.size {
		return fmt.Errorf(
			"physical processing blob %s size %d does not match catalog size %d",
			blob.hash, logicalSize, blob.size,
		)
	}
	read, err := io.Copy(io.Discard, stream)
	if err != nil {
		return fmt.Errorf("reading physical processing blob %s: %w", blob.hash, err)
	}
	if read != blob.size {
		return fmt.Errorf(
			"physical processing blob %s read %d bytes, want %d", blob.hash, read, blob.size,
		)
	}
	if err := stream.Verify(); err != nil {
		return fmt.Errorf("verifying physical processing blob %s: %w", blob.hash, err)
	}
	return nil
}

func validateMetadataRenditionBuild(value metadataRenditionBuild) error {
	if value.Type != metadataRenditionBuildType ||
		value.DeclaredArtifactCount < 0 || value.DeclaredArtifactCount > maxRenditionArtifacts ||
		value.UnitCount < 0 || value.UnitCount > maxRenditionUnits ||
		value.LexicalSegmentCount < 0 || value.LexicalSegmentCount > maxRenditionLexicalSegments {
		return errors.New("invalid rendition build record")
	}
	if err := validateCatalogUTF8(
		value.ProviderOperationID, maxCatalogProviderOpBytes, "provider operation ID", false,
	); err != nil {
		return err
	}
	for name, field := range map[string]string{
		"build ID": value.ID, "source SHA-256": value.SourceSHA256,
		"rendition request fingerprint":        value.RenditionRequestFingerprint,
		"evidence lexical fingerprint":         value.EvidenceLexicalFingerprint,
		"captured artifact policy fingerprint": value.CapturedArtifactPolicyFingerprint,
		"authorization checksum":               value.AuthorizationChecksum,
		"evidence checksum":                    value.EvidenceChecksum, "rendition checksum": value.RenditionChecksum,
		"Markdown checksum": value.MarkdownChecksum,
	} {
		if err := validateCatalogSHA256(field, name); err != nil {
			return err
		}
	}
	if err := validateUUIDv4(value.VaultID); err != nil {
		return err
	}
	policy, err := requireCanonicalProcessingJSON(value.CapturedArtifactPolicy, "captured artifact policy")
	if err != nil {
		return err
	}
	normalizedPolicy, err := normalizeCapturedArtifactPolicyV1(policy)
	if err != nil {
		return err
	}
	if !bytes.Equal(policy, normalizedPolicy.canonical) {
		return errors.New("captured artifact policy is not in canonical role order")
	}
	if digestCatalogJSON(normalizedPolicy.canonical) != value.CapturedArtifactPolicyFingerprint {
		return errors.New("captured artifact policy fingerprint does not match policy JSON")
	}
	receipt, err := requireCanonicalProcessingJSON(value.ProviderReceipt, "provider receipt")
	if err != nil {
		return err
	}
	if len(receipt) > maxProviderReceiptJSONBytes {
		return fmt.Errorf("provider receipt JSON exceeds %d bytes", maxProviderReceiptJSONBytes)
	}
	if len(value.Warnings) > maxRenditionWarnings {
		return fmt.Errorf("rendition build has more than %d warnings", maxRenditionWarnings)
	}
	for _, warning := range value.Warnings {
		if err := validateCatalogUTF8(warning, maxCatalogWarningBytes, "rendition warning", true); err != nil {
			return err
		}
	}
	warningsJSON, err := json.Marshal(value.Warnings, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encoding rendition warnings: %w", err)
	}
	if len(warningsJSON) > maxWarningsJSONBytes {
		return fmt.Errorf("rendition warnings JSON exceeds %d bytes", maxWarningsJSONBytes)
	}
	switch value.Completeness {
	case document.EvidenceComplete, document.EvidencePartial, document.EvidenceDegradedProvenance:
	default:
		return errors.New("invalid rendition completeness")
	}
	return validateMetadataTime("rendition build completed_at", value.CompletedAt)
}

func validateMetadataRenditionArtifact(value metadataRenditionArtifact) error {
	if value.Type != metadataRenditionArtifactType || value.Size < 0 ||
		value.State != RenditionArtifactVerified {
		return errors.New("invalid rendition artifact record")
	}
	if err := validateCatalogUTF8(
		value.ArtifactID, maxCatalogIdentifierBytes, "rendition artifact ID", false,
	); err != nil {
		return err
	}
	if err := validateCatalogUTF8(value.Role, 64, "rendition artifact role", false); err != nil {
		return err
	}
	if !validCapturedArtifactRole(value.Role) {
		return fmt.Errorf("rendition artifact role %q is unknown", value.Role)
	}
	if err := validateCatalogSHA256(value.BuildID, "rendition artifact build ID"); err != nil {
		return err
	}
	if err := validateCatalogSHA256(value.BlobHash, "rendition artifact blob hash"); err != nil {
		return err
	}
	if value.Checksum != value.BlobHash {
		return errors.New("rendition artifact checksum disagrees with blob hash")
	}
	return nil
}

func validateMetadataRenditionUnit(value metadataRenditionUnit) error {
	if value.Type != metadataRenditionUnitType || value.Order < 0 || value.Order >= maxRenditionUnits {
		return errors.New("invalid rendition unit record")
	}
	if err := validateCatalogUTF8(value.UnitID, maxCatalogIdentifierBytes, "rendition unit ID", false); err != nil {
		return err
	}
	if err := validateCatalogUTF8(
		value.EvidenceUnitID, maxCatalogIdentifierBytes, "rendition evidence unit ID", false,
	); err != nil {
		return err
	}
	if err := validateCatalogSHA256(value.BuildID, "rendition unit build ID"); err != nil {
		return err
	}
	if err := validateCatalogSHA256(value.Checksum, "rendition unit checksum"); err != nil {
		return err
	}
	if len(value.HeadingPath) > maxRenditionHeadingDepth {
		return fmt.Errorf("rendition unit has more than %d headings", maxRenditionHeadingDepth)
	}
	for _, heading := range value.HeadingPath {
		if err := validateCatalogHeading(heading); err != nil {
			return err
		}
	}
	if err := validateCatalogLocatorV1(value.Locator); err != nil {
		return err
	}
	headingJSON, err := json.Marshal(value.HeadingPath, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encoding rendition heading path: %w", err)
	}
	if len(headingJSON) > maxProcessingProfileJSONBytes {
		return fmt.Errorf("rendition heading path JSON exceeds %d bytes", maxProcessingProfileJSONBytes)
	}
	locatorJSON, err := json.Marshal(value.Locator, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encoding rendition locator: %w", err)
	}
	if len(locatorJSON) > maxCatalogLocatorJSONBytes {
		return fmt.Errorf("rendition locator JSON exceeds %d bytes", maxCatalogLocatorJSONBytes)
	}
	return nil
}

func validateMetadataRenditionSegment(value metadataRenditionSegment) error {
	if value.Type != metadataRenditionSegmentType || value.Order < 0 ||
		value.Order >= maxRenditionLexicalSegments || value.CharStart < 0 ||
		value.CharEnd < value.CharStart || value.CharEnd-value.CharStart != utf8.RuneCountInString(value.Text) ||
		!utf8.ValidString(value.Text) {
		return errors.New("invalid rendition lexical segment record")
	}
	if err := validateCatalogUTF8(
		value.SegmentID, maxCatalogIdentifierBytes, "rendition lexical segment ID", false,
	); err != nil {
		return err
	}
	if err := validateCatalogUTF8(
		value.UnitID, maxCatalogIdentifierBytes, "rendition lexical unit ID", false,
	); err != nil {
		return err
	}
	if err := validateCatalogUTF8(
		value.Text, maxLexicalSegmentTextBytes, "rendition lexical segment text", true,
	); err != nil {
		return err
	}
	if utf8.RuneCountInString(value.Text) > maxLexicalSegmentRunes {
		return fmt.Errorf("rendition lexical segment text exceeds %d runes", maxLexicalSegmentRunes)
	}
	if err := validateCatalogSHA256(value.BuildID, "rendition lexical segment build ID"); err != nil {
		return err
	}
	return validateCatalogSHA256(value.Checksum, "rendition lexical segment checksum")
}

func validateMetadataRenditionAttachment(value metadataRenditionAttachment) error {
	if value.Type != metadataRenditionAttachType {
		return errors.New("invalid rendition attachment record")
	}
	if err := validateCatalogUTF8(
		value.TrustBoundary, maxCatalogTrustBoundaryBytes, "rendition attachment trust boundary", false,
	); err != nil {
		return err
	}
	for name, field := range map[string]string{
		"attachment ID": value.AttachmentID, "build ID": value.BuildID,
		"processing profile fingerprint":   value.ProcessingProfileFingerprint,
		"retention disclosure fingerprint": value.RetentionDisclosureFingerprint,
		"attachment policy fingerprint":    value.AttachmentPolicyFingerprint,
		"consent fingerprint":              value.ConsentFingerprint,
		"rendition disclosure fingerprint": value.RenditionDisclosureFingerprint,
	} {
		if err := validateCatalogSHA256(field, name); err != nil {
			return err
		}
	}
	if err := validateUUIDv4(value.VaultID); err != nil {
		return err
	}
	if err := validateUUIDv4(value.ContentVersionID); err != nil {
		return err
	}
	return validateMetadataTime("rendition attachment attached_at", value.AttachedAt)
}

func validateProcessingMetadataState(ctx context.Context, tx metadataQuerier) error {
	present, err := processingMetadataSchemaPresent(ctx, tx)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	profileIDs, err := loadProcessingMetadataIDs(
		ctx, tx, "processing profile", `SELECT profile_fingerprint FROM processing_profiles ORDER BY profile_fingerprint`,
	)
	if err != nil {
		return err
	}
	for _, id := range profileIDs {
		profile, err := loadProcessingProfile(ctx, tx, id)
		if err != nil {
			return fmt.Errorf("reading processing profile %s: %w", id, err)
		}
		if _, err := normalizeProcessingProfileRecord(profile); err != nil {
			return fmt.Errorf("invalid processing profile %s: %w", id, err)
		}
	}

	buildIDs, err := loadProcessingMetadataIDs(
		ctx, tx, "rendition build", `SELECT build_id FROM rendition_builds ORDER BY build_id`,
	)
	if err != nil {
		return err
	}
	for _, id := range buildIDs {
		build, err := loadRenditionBuild(ctx, tx, id)
		if err != nil {
			return fmt.Errorf("reading rendition build %s: %w", id, err)
		}
		if _, err := normalizeRenditionBuildRecord(build); err != nil {
			return fmt.Errorf("invalid rendition build %s: %w", id, err)
		}
		if err := validateRenditionBuildStateTx(ctx, tx, id); err != nil {
			return err
		}
	}

	checks := []struct {
		name  string
		query string
	}{
		{"rendition build belongs to another vault", `
			SELECT EXISTS(
			  SELECT 1 FROM rendition_builds b
			  WHERE b.vault_uid != (SELECT vault_uid FROM vault_metadata WHERE singleton=1)
			)`},
		{"rendition attachment profile identity disagrees", `
			SELECT EXISTS(
			  SELECT 1 FROM rendition_attachments a
			  JOIN processing_profiles p ON p.profile_fingerprint=a.profile_fingerprint
			  WHERE a.retention_disclosure_fingerprint != p.retention_disclosure_fingerprint
			     OR a.attachment_policy_fingerprint != p.attachment_policy_fingerprint
			     OR a.consent_fingerprint != p.consent_fingerprint
			     OR a.rendition_disclosure_fingerprint != p.rendition_disclosure_fingerprint
			     OR a.trust_boundary != p.trust_boundary
			)`},
		{"rendition attachment component identity disagrees", `
			SELECT EXISTS(
			  SELECT 1 FROM rendition_attachments a
			  JOIN processing_profiles p ON p.profile_fingerprint=a.profile_fingerprint
			  JOIN rendition_builds b ON b.build_id=a.build_id
			  WHERE b.rendition_request_fingerprint != p.rendition_request_fingerprint
			     OR b.evidence_lexical_fingerprint != p.evidence_lexical_fingerprint
			)`},
		{"rendition attachment source identity disagrees", `
			SELECT EXISTS(
			  SELECT 1 FROM rendition_attachments a
			  JOIN content_versions v ON v.version_id=a.content_version_id
			  JOIN rendition_builds b ON b.build_id=a.build_id
			  WHERE v.blob_hash != b.source_sha256 OR a.vault_uid != b.vault_uid
			)`},
		{"rendition head does not resolve through exact attachment", `
			SELECT EXISTS(
			  SELECT 1 FROM rendition_heads h
			  LEFT JOIN rendition_attachments a
			    ON a.attachment_id=h.attachment_id
			   AND a.content_version_id=h.content_version_id
			   AND a.profile_fingerprint=h.profile_fingerprint
			  WHERE a.attachment_id IS NULL
			)`},
	}
	for _, check := range checks {
		var failed bool
		if err := tx.QueryRowContext(ctx, check.query).Scan(&failed); err != nil {
			return fmt.Errorf("validating processing metadata (%s): %w", check.name, err)
		}
		if failed {
			return errors.New(check.name)
		}
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT attachment_id,vault_uid,content_version_id,build_id,profile_fingerprint,attached_at
		FROM rendition_attachments ORDER BY attachment_id`)
	if err != nil {
		return err
	}
	type attachmentPolicyBinding struct{ buildID, profileID string }
	bindings := make([]attachmentPolicyBinding, 0)
	for rows.Next() {
		var id, vaultID, contentVersionID, buildID, profileID, attachedAt string
		if err := rows.Scan(&id, &vaultID, &contentVersionID, &buildID, &profileID, &attachedAt); err != nil {
			_ = rows.Close()
			return err
		}
		if err := validateCatalogSHA256(id, "rendition attachment ID"); err != nil {
			_ = rows.Close()
			return err
		}
		if err := validateMetadataTime("rendition attachment attached_at", attachedAt); err != nil {
			_ = rows.Close()
			return err
		}
		bindings = append(bindings, attachmentPolicyBinding{buildID: buildID, profileID: profileID})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, binding := range bindings {
		profile, err := loadProcessingProfile(ctx, tx, binding.profileID)
		if err != nil {
			return err
		}
		build, err := loadRenditionBuild(ctx, tx, binding.buildID)
		if err != nil {
			return err
		}
		if err := validateRenditionArtifactRolesForProfile(profile, build); err != nil {
			return fmt.Errorf("invalid restored rendition attachment: %w", err)
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT content_version_id,profile_fingerprint,attachment_id,published_at FROM rendition_heads`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var record RenditionHeadRecord
		if err := rows.Scan(&record.ContentVersionID, &record.ProcessingProfileFingerprint,
			&record.AttachmentID, &record.PublishedAt); err != nil {
			return err
		}
		if err := validateRenditionHeadRecord(record); err != nil {
			return err
		}
	}
	return rows.Err()
}

func requireCanonicalProcessingJSON(raw jsontext.Value, subject string) (jsontext.Value, error) {
	canonical, err := canonicalCatalogJSON(raw, subject)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(raw, canonical) {
		return nil, fmt.Errorf("%s JSON is not canonical", subject)
	}
	return canonical, nil
}

func processingMetadataSchemaPresent(ctx context.Context, tx metadataQuerier) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_schema
		WHERE type='table' AND name IN (
			'processing_profiles','rendition_builds','rendition_artifacts',
			'rendition_units','rendition_lexical_segments',
			'rendition_attachments','rendition_heads'
		)`).Scan(&count); err != nil {
		return false, fmt.Errorf("detecting processing metadata schema: %w", err)
	}
	if count == 0 {
		return false, nil
	}
	if count != 7 {
		return false, fmt.Errorf("processing metadata schema is incomplete: found %d of 7 tables", count)
	}
	return true, nil
}

func loadProcessingMetadataIDs(
	ctx context.Context, tx metadataQuerier, subject, query string,
) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("validating %ss: %w", subject, err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning %s identity: %w", subject, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating %s identities: %w", subject, err)
	}
	return ids, nil
}
