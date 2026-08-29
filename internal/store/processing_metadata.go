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
	"slices"
	"unicode/utf8"

	"go.kenn.io/kit/packstore"

	"go.kenn.io/docbank/document"
)

const (
	metadataProcessingProfileType          = "processing_profile"
	metadataRenditionBuildType             = "rendition_build"
	metadataRenditionArtifactType          = "rendition_artifact"
	metadataRenditionUnitType              = "rendition_unit"
	metadataRenditionSegmentType           = "rendition_lexical_segment"
	metadataRenditionAttachType            = "rendition_attachment"
	metadataRenditionHeadType              = "rendition_head"
	metadataLexicalGenerationType          = "rendition_lexical_generation"
	metadataCurrentRenditionRootType       = "current_rendition_root"
	metadataDerivativePurgeSuppressionType = "derivative_purge_suppression"
	metadataProcessingIncarnationType      = "processing_incarnation"
	metadataProcessingConsentGrantType     = "processing_consent_grant"
	metadataProcessingConsentRevokeType    = "processing_consent_revocation"
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

type metadataLexicalGeneration struct {
	Type           string   `json:"type"`
	GenerationID   string   `json:"generation_id"`
	SegmentCount   int      `json:"segment_count"`
	ManifestDigest string   `json:"manifest_digest"`
	BuildIDs       []string `json:"build_ids"`
	BuildDigest    string   `json:"build_digest"`
	BuiltAt        string   `json:"built_at"`
	Headed         bool     `json:"headed"`
}

type metadataCurrentRenditionRoot struct {
	Type         string                     `json:"type"`
	ID           string                     `json:"root_id"`
	Kind         CurrentRenditionRootKind   `json:"root_kind"`
	TargetKind   CurrentRenditionTargetKind `json:"target_kind"`
	TargetID     string                     `json:"target_id"`
	FencingToken int64                      `json:"fencing_token"`
	RecordedAt   string                     `json:"recorded_at"`
	Active       bool                       `json:"active"`
	ReleasedAt   *string                    `json:"released_at"`
}

type metadataDerivativePurgeSuppression struct {
	Type               string  `json:"type"`
	SourceSHA256       string  `json:"source_sha256"`
	ProfileFingerprint string  `json:"profile_fingerprint"`
	BuildID            string  `json:"build_id"`
	PurgedAt           string  `json:"purged_at"`
	Active             bool    `json:"active"`
	SupersededAt       *string `json:"superseded_at"`
	SupersedingBuildID *string `json:"superseding_build_id"`
}

type metadataProcessingIncarnation struct {
	Type      string `json:"type"`
	ID        string `json:"incarnation_id"`
	CreatedAt string `json:"created_at"`
}

type metadataProcessingConsentGrant struct {
	Type                    string   `json:"type"`
	ID                      string   `json:"grant_id"`
	VaultID                 string   `json:"vault_id"`
	ProcessingIncarnationID string   `json:"incarnation_id"`
	Principal               string   `json:"principal"`
	Scope                   string   `json:"scope"`
	ProfileFingerprint      string   `json:"profile_fingerprint"`
	DisclosureFingerprint   string   `json:"disclosure_fingerprint"`
	InputClasses            []string `json:"input_classes"`
	RetainedArtifactClasses []string `json:"retained_artifact_classes"`
	RevocationFence         int64    `json:"revocation_fence"`
	IssuedAt                string   `json:"issued_at"`
	ExpiresAt               *string  `json:"expires_at"`
}

type metadataProcessingConsentRevocation struct {
	Type                    string `json:"type"`
	ID                      string `json:"revocation_id"`
	VaultID                 string `json:"vault_id"`
	ProcessingIncarnationID string `json:"incarnation_id"`
	Principal               string `json:"principal"`
	Scope                   string `json:"scope"`
	Fence                   int64  `json:"fence"`
	RevokedAt               string `json:"revoked_at"`
}

var processingMetadataRequiredFields = map[string][]string{
	metadataProcessingIncarnationType: {
		metadataTypeField, "incarnation_id", metadataCreatedAtField,
	},
	metadataProcessingConsentGrantType: {
		metadataTypeField, "grant_id", auditVaultIDField, "incarnation_id",
		"principal", "scope", "profile_fingerprint", "disclosure_fingerprint",
		"input_classes", "retained_artifact_classes", "revocation_fence",
		"issued_at", "expires_at",
	},
	metadataProcessingConsentRevokeType: {
		metadataTypeField, "revocation_id", auditVaultIDField, "incarnation_id",
		"principal", "scope", "fence", "revoked_at",
	},
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
	metadataLexicalGenerationType: {
		metadataTypeField, "generation_id", "segment_count", "manifest_digest",
		"build_ids", "build_digest", "built_at", "headed",
	},
	metadataCurrentRenditionRootType: {
		metadataTypeField, "root_id", "root_kind", "target_kind", "target_id",
		"fencing_token", "recorded_at", "active", "released_at",
	},
	metadataDerivativePurgeSuppressionType: {
		metadataTypeField, "source_sha256", "profile_fingerprint", "build_id",
		"purged_at", "active", "superseded_at", "superseding_build_id",
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
	if err := exportProcessingConsent(ctx, tx, write); err != nil {
		return err
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
	if err := exportRenditionHeads(ctx, tx, write); err != nil {
		return err
	}
	if err := exportLexicalGenerations(ctx, tx, write); err != nil {
		return err
	}
	if err := exportDurableCurrentRenditionRoots(ctx, tx, write); err != nil {
		return err
	}
	return exportDerivativePurgeSuppressions(ctx, tx, write)
}

func exportProcessingConsent(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	if err := exportProcessingIncarnations(ctx, tx, write); err != nil {
		return err
	}
	if err := exportProcessingConsentRevocations(ctx, tx, write); err != nil {
		return err
	}
	return exportProcessingConsentGrants(ctx, tx, write)
}

func exportProcessingIncarnations(ctx context.Context, tx metadataQuerier, write metadataWrite) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT i.incarnation_id,i.created_at
		FROM processing_incarnations i
		WHERE EXISTS(SELECT 1 FROM processing_consent_grants g
		             WHERE g.incarnation_id=i.incarnation_id)
		   OR EXISTS(SELECT 1 FROM processing_consent_revocations r
		             WHERE r.incarnation_id=i.incarnation_id)
		ORDER BY i.incarnation_id`)
	if err != nil {
		return fmt.Errorf("exporting processing incarnations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		value := metadataProcessingIncarnation{Type: metadataProcessingIncarnationType}
		if err := rows.Scan(&value.ID, &value.CreatedAt); err != nil {
			return fmt.Errorf("scanning processing incarnation metadata: %w", err)
		}
		if err := write(value); err != nil {
			return err
		}
	}
	return rowsError("processing incarnation", rows)
}

func exportProcessingConsentRevocations(
	ctx context.Context, tx metadataQuerier, write metadataWrite,
) error {
	revocations, err := tx.QueryContext(ctx, `
		SELECT revocation_id,vault_uid,incarnation_id,principal,scope,fence,revoked_at
		FROM processing_consent_revocations ORDER BY incarnation_id,principal,scope,fence`)
	if err != nil {
		return fmt.Errorf("exporting processing consent revocations: %w", err)
	}
	defer func() { _ = revocations.Close() }()
	for revocations.Next() {
		value := metadataProcessingConsentRevocation{Type: metadataProcessingConsentRevokeType}
		if err := revocations.Scan(&value.ID, &value.VaultID, &value.ProcessingIncarnationID,
			&value.Principal, &value.Scope, &value.Fence, &value.RevokedAt); err != nil {
			return fmt.Errorf("scanning processing consent revocation metadata: %w", err)
		}
		if err := write(value); err != nil {
			return err
		}
	}
	return rowsError("processing consent revocation", revocations)
}

func exportProcessingConsentGrants(
	ctx context.Context, tx metadataQuerier, write metadataWrite,
) error {
	grants, err := tx.QueryContext(ctx, `
		SELECT grant_id,vault_uid,incarnation_id,principal,scope,profile_fingerprint,
		       disclosure_fingerprint,input_classes_json,retained_classes_json,
		       revocation_fence,issued_at,expires_at
		FROM processing_consent_grants ORDER BY incarnation_id,issued_at,grant_id`)
	if err != nil {
		return fmt.Errorf("exporting processing consent grants: %w", err)
	}
	defer func() { _ = grants.Close() }()
	for grants.Next() {
		value := metadataProcessingConsentGrant{Type: metadataProcessingConsentGrantType}
		var inputs, retained string
		var expires sql.NullString
		if err := grants.Scan(&value.ID, &value.VaultID, &value.ProcessingIncarnationID,
			&value.Principal, &value.Scope, &value.ProfileFingerprint,
			&value.DisclosureFingerprint, &inputs, &retained, &value.RevocationFence,
			&value.IssuedAt, &expires); err != nil {
			return fmt.Errorf("scanning processing consent grant metadata: %w", err)
		}
		if err := json.Unmarshal([]byte(inputs), &value.InputClasses); err != nil {
			return fmt.Errorf("decoding processing consent input classes: %w", err)
		}
		if err := json.Unmarshal([]byte(retained), &value.RetainedArtifactClasses); err != nil {
			return fmt.Errorf("decoding processing consent retained classes: %w", err)
		}
		if expires.Valid {
			value.ExpiresAt = &expires.String
		}
		if err := write(value); err != nil {
			return err
		}
	}
	return rowsError("processing consent grant", grants)
}

func exportLexicalGenerations(
	ctx context.Context, tx metadataQuerier, write metadataWrite,
) error {
	present, err := lexicalGenerationSchemaPresentTx(ctx, tx)
	if err != nil || !present {
		return err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT g.generation_id,g.segment_count,m.manifest_digest,m.build_digest,g.built_at,
		       EXISTS(SELECT 1 FROM rendition_lexical_heads h
		              WHERE h.generation_id=g.generation_id)
		FROM rendition_lexical_generations g
		JOIN rendition_lexical_generation_manifests m ON m.generation_id=g.generation_id
		WHERE EXISTS(
		         SELECT 1 FROM rendition_lexical_heads h WHERE h.generation_id=g.generation_id
		      ) OR EXISTS(
		         SELECT 1 FROM current_rendition_roots r
		         WHERE r.target_kind='lexical_generation' AND r.target_id=g.generation_id
		           AND r.root_kind IN ('retention','audit')
		      )
		ORDER BY g.generation_id`)
	if err != nil {
		return fmt.Errorf("exporting lexical generations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var records []metadataLexicalGeneration
	for rows.Next() {
		record := metadataLexicalGeneration{Type: metadataLexicalGenerationType}
		if err := rows.Scan(&record.GenerationID, &record.SegmentCount,
			&record.ManifestDigest, &record.BuildDigest, &record.BuiltAt, &record.Headed); err != nil {
			return fmt.Errorf("scanning lexical generation metadata: %w", err)
		}
		record.BuildIDs, err = lexicalGenerationBuildIDsQuery(ctx, tx, record.GenerationID)
		if err != nil {
			return err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("exporting lexical generations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("closing lexical generation metadata: %w", err)
	}
	for _, record := range records {
		if err := write(record); err != nil {
			return err
		}
	}
	return nil
}

func lexicalGenerationBuildIDsQuery(
	ctx context.Context, tx metadataQuerier, generationID string,
) (_ []string, retErr error) {
	rows, err := tx.QueryContext(ctx, `SELECT build_id
		FROM rendition_lexical_generation_builds
		WHERE generation_id=? ORDER BY build_id`, generationID)
	if err != nil {
		return nil, fmt.Errorf("reading lexical generation %s membership: %w", generationID, err)
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	var buildIDs []string
	for rows.Next() {
		var buildID string
		if err := rows.Scan(&buildID); err != nil {
			return nil, fmt.Errorf("scanning lexical generation %s membership: %w", generationID, err)
		}
		buildIDs = append(buildIDs, buildID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading lexical generation %s membership: %w", generationID, err)
	}
	return buildIDs, nil
}

func exportDurableCurrentRenditionRoots(
	ctx context.Context, tx metadataQuerier, write metadataWrite,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT root_id,root_kind,target_kind,target_id,fencing_token,recorded_at,
		       active,released_at
		FROM current_rendition_roots
		WHERE root_kind IN ('retention','audit')
		ORDER BY root_id`)
	if err != nil {
		return fmt.Errorf("exporting durable current rendition roots: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := metadataCurrentRenditionRoot{Type: metadataCurrentRenditionRootType}
		var released sql.NullString
		if err := rows.Scan(&record.ID, &record.Kind, &record.TargetKind, &record.TargetID,
			&record.FencingToken, &record.RecordedAt, &record.Active, &released); err != nil {
			return fmt.Errorf("scanning durable current rendition root metadata: %w", err)
		}
		if released.Valid {
			record.ReleasedAt = &released.String
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rowsError("durable current rendition root", rows)
}

func exportDerivativePurgeSuppressions(
	ctx context.Context, tx metadataQuerier, write metadataWrite,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT source_sha256,profile_fingerprint,build_id,purged_at,active,
		       superseded_at,superseding_build_id
		FROM derivative_purge_suppressions
		ORDER BY source_sha256,profile_fingerprint,build_id`)
	if err != nil {
		return fmt.Errorf("exporting derivative purge suppressions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record := metadataDerivativePurgeSuppression{Type: metadataDerivativePurgeSuppressionType}
		var supersededAt, supersedingBuildID sql.NullString
		if err := rows.Scan(&record.SourceSHA256, &record.ProfileFingerprint, &record.BuildID,
			&record.PurgedAt, &record.Active, &supersededAt, &supersedingBuildID); err != nil {
			return fmt.Errorf("scanning derivative purge suppression metadata: %w", err)
		}
		if supersededAt.Valid {
			record.SupersededAt = &supersededAt.String
		}
		if supersedingBuildID.Valid {
			record.SupersedingBuildID = &supersedingBuildID.String
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return rowsError("derivative purge suppression", rows)
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
	case metadataProcessingIncarnationType:
		var value metadataProcessingIncarnation
		if err := decodeMetadataRecord(raw, &value); err != nil {
			return err
		}
		if err := validateMetadataProcessingIncarnation(value); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO processing_incarnations(incarnation_id,created_at) VALUES(?,?)`,
			value.ID, value.CreatedAt)
		return err
	case metadataProcessingConsentRevokeType:
		var value metadataProcessingConsentRevocation
		if err := decodeMetadataRecord(raw, &value); err != nil {
			return err
		}
		if err := validateMetadataProcessingConsentRevocation(value); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO processing_consent_revocations(
			revocation_id,vault_uid,incarnation_id,principal,scope,fence,revoked_at
		) VALUES(?,?,?,?,?,?,?)`, value.ID, value.VaultID, value.ProcessingIncarnationID,
			value.Principal, value.Scope, value.Fence, value.RevokedAt)
		return err
	case metadataProcessingConsentGrantType:
		var value metadataProcessingConsentGrant
		if err := decodeMetadataRecord(raw, &value); err != nil {
			return err
		}
		authority, err := validateMetadataProcessingConsentGrant(value)
		if err != nil {
			return err
		}
		var expires any
		if value.ExpiresAt != nil {
			expires = *value.ExpiresAt
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO processing_consent_grants(
			grant_id,vault_uid,incarnation_id,principal,scope,profile_fingerprint,
			disclosure_fingerprint,input_classes_json,retained_classes_json,
			revocation_fence,issued_at,expires_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, value.ID, value.VaultID,
			value.ProcessingIncarnationID, authority.principal, authority.scope,
			authority.profile, authority.disclosure, authority.inputsJSON,
			authority.retainedJSON, value.RevocationFence, value.IssuedAt, expires)
		return err
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
	case metadataLexicalGenerationType:
		var value metadataLexicalGeneration
		if err := decodeMetadataRecord(raw, &value); err != nil {
			return err
		}
		return restoreLexicalGenerationTx(ctx, tx, value)
	case metadataCurrentRenditionRootType:
		var value metadataCurrentRenditionRoot
		if err := decodeMetadataRecord(raw, &value); err != nil {
			return err
		}
		root := CurrentRenditionRoot{
			ID: value.ID, Kind: value.Kind, TargetKind: value.TargetKind,
			TargetID: value.TargetID, FencingToken: value.FencingToken,
			RecordedAt: value.RecordedAt,
		}
		if err := validateDurableCurrentRenditionRootMetadata(value, root); err != nil {
			return err
		}
		if value.Active {
			if err := requireCurrentRenditionTargetTx(ctx, tx, root); err != nil {
				return err
			}
		}
		var released any
		if value.ReleasedAt != nil {
			released = *value.ReleasedAt
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO current_rendition_roots(
				root_id,root_kind,target_kind,target_id,fencing_token,recorded_at,expires_at,
				active,released_at
			) VALUES(?,?,?,?,?,?,NULL,?,?)`, root.ID, root.Kind, root.TargetKind,
			root.TargetID, root.FencingToken, root.RecordedAt, value.Active, released)
		return err
	case metadataDerivativePurgeSuppressionType:
		var value metadataDerivativePurgeSuppression
		if err := decodeMetadataRecord(raw, &value); err != nil {
			return err
		}
		if err := validateMetadataDerivativePurgeSuppression(value); err != nil {
			return err
		}
		var supersededAt, supersedingBuildID any
		if value.SupersededAt != nil {
			supersededAt = *value.SupersededAt
		}
		if value.SupersedingBuildID != nil {
			supersedingBuildID = *value.SupersedingBuildID
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO derivative_purge_suppressions(
				source_sha256,profile_fingerprint,build_id,purged_at,active,
				superseded_at,superseding_build_id
			) VALUES(?,?,?,?,?,?,?)`, value.SourceSHA256, value.ProfileFingerprint,
			value.BuildID, value.PurgedAt, value.Active, supersededAt, supersedingBuildID)
		return err
	default:
		return fmt.Errorf("unknown processing metadata type %q", kind)
	}
}

func restoreLexicalGenerationTx(
	ctx context.Context, tx *sql.Tx, value metadataLexicalGeneration,
) error {
	if err := validateMetadataLexicalGeneration(value); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, lexicalProjectionSchema); err != nil {
		return fmt.Errorf("initializing restored lexical projection: %w", err)
	}
	segments := make([]lexicalManifestRow, 0, value.SegmentCount)
	for _, buildID := range value.BuildIDs {
		buildSegments, err := readCatalogLexicalManifestRowsTx(ctx, tx, buildID)
		if err != nil {
			return err
		}
		segments = append(segments, buildSegments...)
	}
	if len(segments) != value.SegmentCount ||
		lexicalManifestDigest(segments) != value.ManifestDigest ||
		lexicalBuildDigest(value.BuildIDs) != value.BuildDigest {
		return fmt.Errorf("restored lexical generation %s has a different immutable manifest",
			value.GenerationID)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO rendition_lexical_generations(generation_id,segment_count,build_count,built_at)
		VALUES(?,?,?,?)`, value.GenerationID, value.SegmentCount, len(value.BuildIDs), value.BuiltAt,
	); err != nil {
		return fmt.Errorf("restoring lexical generation %s: %w", value.GenerationID, err)
	}
	for _, buildID := range value.BuildIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO rendition_lexical_generation_builds(
			generation_id,build_id) VALUES(?,?)`, value.GenerationID, buildID); err != nil {
			return fmt.Errorf("restoring lexical generation %s build %s: %w",
				value.GenerationID, buildID, err)
		}
	}
	for _, segment := range segments {
		if _, err := tx.ExecContext(ctx, `INSERT INTO rendition_lexical_fts(
			generation_id,build_id,segment_id,text) VALUES(?,?,?,?)`,
			value.GenerationID, segment.buildID, segment.segmentID, segment.text); err != nil {
			return fmt.Errorf("rebuilding restored lexical generation %s: %w",
				value.GenerationID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO rendition_lexical_generation_manifests(
		generation_id,manifest_digest,build_digest) VALUES(?,?,?)`,
		value.GenerationID, value.ManifestDigest, value.BuildDigest); err != nil {
		return fmt.Errorf("restoring lexical generation %s manifest: %w", value.GenerationID, err)
	}
	if value.Headed {
		if _, err := tx.ExecContext(ctx, `INSERT INTO rendition_lexical_heads(
			singleton,generation_id) VALUES(1,?)`, value.GenerationID); err != nil {
			return fmt.Errorf("restoring lexical generation %s head: %w", value.GenerationID, err)
		}
	}
	return nil
}

func validateMetadataLexicalGeneration(value metadataLexicalGeneration) error {
	if value.Type != metadataLexicalGenerationType || value.SegmentCount < 0 ||
		value.SegmentCount > maxRenditionLexicalSegments ||
		len(value.BuildIDs) > maxRenditionLexicalSegments {
		return errors.New("invalid lexical generation metadata")
	}
	for subject, digest := range map[string]string{
		"lexical generation ID":   value.GenerationID,
		"lexical manifest digest": value.ManifestDigest,
		"lexical build digest":    value.BuildDigest,
	} {
		if err := validateCatalogSHA256(digest, subject); err != nil {
			return err
		}
	}
	if err := validateMetadataTime("lexical generation built_at", value.BuiltAt); err != nil {
		return err
	}
	if !slices.IsSorted(value.BuildIDs) {
		return errors.New("lexical generation build IDs are not canonical")
	}
	for index, buildID := range value.BuildIDs {
		if err := validateCatalogSHA256(buildID, "lexical generation build ID"); err != nil {
			return err
		}
		if index > 0 && buildID == value.BuildIDs[index-1] {
			return errors.New("lexical generation build IDs are duplicated")
		}
	}
	return nil
}

func validateMetadataDerivativePurgeSuppression(
	value metadataDerivativePurgeSuppression,
) error {
	if value.Type != metadataDerivativePurgeSuppressionType ||
		value.Active == (value.SupersededAt != nil || value.SupersedingBuildID != nil) ||
		(value.SupersededAt == nil) != (value.SupersedingBuildID == nil) {
		return errors.New("invalid derivative purge suppression record")
	}
	for name, digest := range map[string]string{
		"source SHA-256": value.SourceSHA256, "profile fingerprint": value.ProfileFingerprint,
		"build ID": value.BuildID,
	} {
		if err := validateCatalogSHA256(digest, name); err != nil {
			return err
		}
	}
	if err := validateMetadataTime("derivative purge suppression purged_at", value.PurgedAt); err != nil {
		return err
	}
	if value.SupersededAt != nil {
		if err := validateMetadataTime(
			"derivative purge suppression superseded_at", *value.SupersededAt); err != nil {
			return err
		}
		return validateCatalogSHA256(*value.SupersedingBuildID, "superseding build ID")
	}
	return nil
}

func validateMetadataProcessingIncarnation(value metadataProcessingIncarnation) error {
	if value.Type != metadataProcessingIncarnationType {
		return errors.New("invalid processing incarnation record")
	}
	if err := validateUUIDv4(value.ID); err != nil {
		return fmt.Errorf("invalid processing incarnation ID: %w", err)
	}
	return validateMetadataTime("processing incarnation created_at", value.CreatedAt)
}

func validateMetadataProcessingConsentRevocation(
	value metadataProcessingConsentRevocation,
) error {
	if value.Type != metadataProcessingConsentRevokeType || value.Fence <= 0 {
		return errors.New("invalid processing consent revocation record")
	}
	for subject, id := range map[string]string{
		"revocation ID": value.ID, "processing incarnation ID": value.ProcessingIncarnationID,
		"vault ID": value.VaultID,
	} {
		if err := validateUUIDv4(id); err != nil {
			return fmt.Errorf("invalid processing consent %s: %w", subject, err)
		}
	}
	if _, err := normalizeConsentLabel("principal", value.Principal); err != nil {
		return err
	}
	if _, err := normalizeConsentLabel("scope", value.Scope); err != nil {
		return err
	}
	return validateMetadataTime("processing consent revoked_at", value.RevokedAt)
}

func validateMetadataProcessingConsentGrant(
	value metadataProcessingConsentGrant,
) (normalizedConsentAuthority, error) {
	if value.Type != metadataProcessingConsentGrantType || value.RevocationFence < 0 {
		return normalizedConsentAuthority{}, errors.New("invalid processing consent grant record")
	}
	for subject, id := range map[string]string{
		"grant ID": value.ID, "processing incarnation ID": value.ProcessingIncarnationID,
		"vault ID": value.VaultID,
	} {
		if err := validateUUIDv4(id); err != nil {
			return normalizedConsentAuthority{}, fmt.Errorf("invalid processing consent %s: %w", subject, err)
		}
	}
	if value.RetainedArtifactClasses == nil {
		return normalizedConsentAuthority{}, errors.New("processing consent retained artifact classes cannot be null")
	}
	authority, err := normalizeConsentAuthority(ProviderOperationAuthorizationRequest{
		Principal: value.Principal, Scope: value.Scope,
		ProfileFingerprint:      value.ProfileFingerprint,
		DisclosureFingerprint:   value.DisclosureFingerprint,
		InputClasses:            value.InputClasses,
		RetainedArtifactClasses: value.RetainedArtifactClasses,
	})
	if err != nil {
		return normalizedConsentAuthority{}, err
	}
	if !slices.Equal(authority.inputs, value.InputClasses) ||
		!slices.Equal(authority.retained, value.RetainedArtifactClasses) {
		return normalizedConsentAuthority{}, errors.New("processing consent class sets are not canonical")
	}
	if err := validateMetadataTime("processing consent issued_at", value.IssuedAt); err != nil {
		return normalizedConsentAuthority{}, err
	}
	if value.ExpiresAt != nil {
		if err := validateMetadataTime("processing consent expires_at", *value.ExpiresAt); err != nil {
			return normalizedConsentAuthority{}, err
		}
	}
	return authority, nil
}

func validateDurableCurrentRenditionRootMetadata(
	value metadataCurrentRenditionRoot, root CurrentRenditionRoot,
) error {
	if value.Type != metadataCurrentRenditionRootType ||
		(root.Kind != RenditionRootRetention && root.Kind != RenditionRootAudit) {
		return errors.New("invalid durable current rendition root record")
	}
	if err := validateCurrentRenditionRoot(root); err != nil {
		return err
	}
	if value.Active == (value.ReleasedAt != nil) {
		return errors.New("durable current rendition root active state is inconsistent")
	}
	if value.ReleasedAt != nil {
		return validateMetadataTime("current rendition root released_at", *value.ReleasedAt)
	}
	return nil
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

// VerifyRenditionBlobBytes verifies every retained rendition source and
// artifact through the restored mixed-storage catalog, including builds that
// are staged but not attached to an active head.
func (s *Store) VerifyRenditionBlobBytes(ctx context.Context, reader RenditionBlobReader) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.source_sha256, source.size
		FROM rendition_builds b
		JOIN blobs source ON source.hash=b.source_sha256
		UNION
		SELECT artifact.blob_hash, artifact.size
		FROM rendition_artifacts artifact
		ORDER BY 1, 2`)
	if err != nil {
		return fmt.Errorf("listing retained rendition bytes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var blob importedProcessingBlob
		if err := rows.Scan(&blob.hash, &blob.size); err != nil {
			return fmt.Errorf("scanning retained rendition bytes: %w", err)
		}
		if err := verifyRenditionBlob(ctx, reader, blob); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating retained rendition bytes: %w", err)
	}
	return nil
}

// VerifyRenditionBlobAuthority verifies the relational and physical-catalog
// location authority for every retained rendition source and artifact,
// including staged builds that are not currently attached to a document
// version. VerifyRenditionBlobBytes separately reads and verifies each location.
func (s *Store) VerifyRenditionBlobAuthority(ctx context.Context) (retErr error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("starting processing blob verification: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, tx.Rollback()) }()
	if err := validateProcessingMetadataState(ctx, tx); err != nil {
		return fmt.Errorf("validating processing blob authority: %w", err)
	}
	if err := verifyRenditionBlobCatalogAuthority(ctx, tx); err != nil {
		return fmt.Errorf("verifying processing blob authority: %w", err)
	}
	return nil
}

func verifyRenditionBlobCatalogAuthority(ctx context.Context, tx *sql.Tx) (retErr error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT source_sha256 FROM rendition_builds
		UNION
		SELECT blob_hash FROM rendition_artifacts
		ORDER BY source_sha256`)
	if err != nil {
		return fmt.Errorf("reading processing blob catalog authority: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return fmt.Errorf("scanning processing blob catalog authority: %w", err)
		}
		if _, err := requirePhysicalAuthorityTx(tx, hash); err != nil {
			return fmt.Errorf("processing blob %s: %w", hash, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating processing blob catalog authority: %w", err)
	}
	return nil
}

// RebuildRenditionLexicalProjection reconstructs the excluded FTS projection
// solely from restored catalog rows. No provider or network access is involved.
func (s *Store) RebuildRenditionLexicalProjection(ctx context.Context) error {
	var generationID string
	err := s.withStorageTx(ctx, func(tx *sql.Tx) error {
		present, err := processingMetadataSchemaPresent(ctx, tx)
		if err != nil {
			return err
		}
		if !present {
			return nil
		}
		var activeGenerationID string
		err = tx.QueryRowContext(ctx, `SELECT generation_id
			FROM rendition_lexical_heads WHERE singleton=1`).Scan(&activeGenerationID)
		if err == nil {
			if _, err := loadAndValidateLexicalGenerationTx(ctx, tx, activeGenerationID); err != nil {
				return fmt.Errorf("validating restored lexical projection: %w", err)
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) && !isMissingLexicalSchema(err) {
			return fmt.Errorf("reading restored lexical projection head: %w", err)
		}
		var heads int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM rendition_heads`).Scan(&heads); err != nil {
			return fmt.Errorf("counting rendition heads for lexical rebuild: %w", err)
		}
		if heads == 0 {
			return nil
		}
		rows, err := readCatalogLexicalManifestRowsTx(ctx, tx, "")
		if err != nil {
			return err
		}
		buildIDs, err := lexicalCatalogBuildIDsTx(ctx, tx)
		if err != nil {
			return err
		}
		generationID = lexicalReplacementGenerationID(rows, buildIDs)
		return nil
	})
	if err != nil || generationID == "" {
		return err
	}
	if _, err := s.StageLexicalGeneration(ctx, generationID); err != nil {
		return fmt.Errorf("rebuilding restored lexical projection: %w", err)
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_lexical_heads(singleton,generation_id) VALUES(1,?)
			ON CONFLICT(singleton) DO UPDATE SET generation_id=excluded.generation_id`, generationID,
		); err != nil {
			return fmt.Errorf("publishing rebuilt lexical projection: %w", err)
		}
		return nil
	})
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
	if err := validateProcessingConsentState(ctx, tx); err != nil {
		return err
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
	if err := rows.Err(); err != nil {
		return err
	}
	return validateCurrentRenditionRootState(ctx, tx)
}

func validateProcessingConsentState(ctx context.Context, tx metadataQuerier) error {
	var currentCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM current_processing_incarnation`).Scan(
		&currentCount); err != nil {
		return fmt.Errorf("validating current processing incarnation: %w", err)
	}
	if currentCount != 1 {
		return fmt.Errorf("current processing incarnation has %d rows", currentCount)
	}

	if err := validateProcessingIncarnations(ctx, tx); err != nil {
		return err
	}
	if err := validateProcessingConsentRevocations(ctx, tx); err != nil {
		return err
	}
	if err := validateProcessingConsentGrants(ctx, tx); err != nil {
		return err
	}

	var invalid bool
	checks := []struct {
		name  string
		query string
	}{
		{"processing consent belongs to another vault", `
			SELECT EXISTS(
			  SELECT 1 FROM processing_consent_grants
			  WHERE vault_uid != (SELECT vault_uid FROM vault_metadata WHERE singleton=1)
			  UNION ALL
			  SELECT 1 FROM processing_consent_revocations
			  WHERE vault_uid != (SELECT vault_uid FROM vault_metadata WHERE singleton=1)
			)`},
		{"processing consent revocation fence is not contiguous", `
			SELECT EXISTS(
			  SELECT 1 FROM processing_consent_revocations
			  GROUP BY vault_uid,incarnation_id,principal,scope
			  HAVING MIN(fence) != 1 OR MAX(fence) != COUNT(*)
			)`},
		{"processing consent grant has an impossible revocation fence", `
			SELECT EXISTS(
			  SELECT 1 FROM processing_consent_grants g
			  WHERE g.revocation_fence > COALESCE((
			    SELECT MAX(r.fence) FROM processing_consent_revocations r
			    WHERE r.vault_uid=g.vault_uid AND r.incarnation_id=g.incarnation_id
			      AND r.principal=g.principal AND r.scope=g.scope
			  ),0)
			)`},
	}
	for _, check := range checks {
		if err := tx.QueryRowContext(ctx, check.query).Scan(&invalid); err != nil {
			return fmt.Errorf("validating processing consent (%s): %w", check.name, err)
		}
		if invalid {
			return errors.New(check.name)
		}
	}
	return nil
}

func validateProcessingConsentRevocations(ctx context.Context, tx metadataQuerier) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT revocation_id,vault_uid,incarnation_id,principal,scope,fence,revoked_at
		FROM processing_consent_revocations ORDER BY incarnation_id,principal,scope,fence`)
	if err != nil {
		return fmt.Errorf("validating processing consent revocations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		value := metadataProcessingConsentRevocation{Type: metadataProcessingConsentRevokeType}
		if err := rows.Scan(&value.ID, &value.VaultID, &value.ProcessingIncarnationID,
			&value.Principal, &value.Scope, &value.Fence, &value.RevokedAt); err != nil {
			return fmt.Errorf("scanning processing consent revocation metadata: %w", err)
		}
		if err := validateMetadataProcessingConsentRevocation(value); err != nil {
			return err
		}
	}
	return rowsError("processing consent revocation", rows)
}

func validateProcessingConsentGrants(ctx context.Context, tx metadataQuerier) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT grant_id,vault_uid,incarnation_id,principal,scope,profile_fingerprint,
		       disclosure_fingerprint,input_classes_json,retained_classes_json,
		       revocation_fence,issued_at,expires_at
		FROM processing_consent_grants ORDER BY incarnation_id,issued_at,grant_id`)
	if err != nil {
		return fmt.Errorf("validating processing consent grants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		value := metadataProcessingConsentGrant{Type: metadataProcessingConsentGrantType}
		var inputs, retained string
		var expires sql.NullString
		if err := rows.Scan(&value.ID, &value.VaultID, &value.ProcessingIncarnationID,
			&value.Principal, &value.Scope, &value.ProfileFingerprint,
			&value.DisclosureFingerprint, &inputs, &retained, &value.RevocationFence,
			&value.IssuedAt, &expires); err != nil {
			return fmt.Errorf("scanning processing consent grant metadata: %w", err)
		}
		if err := json.Unmarshal([]byte(inputs), &value.InputClasses); err != nil {
			return fmt.Errorf("decoding processing consent input classes: %w", err)
		}
		if err := json.Unmarshal([]byte(retained), &value.RetainedArtifactClasses); err != nil {
			return fmt.Errorf("decoding processing consent retained classes: %w", err)
		}
		if expires.Valid {
			value.ExpiresAt = &expires.String
		}
		if _, err := validateMetadataProcessingConsentGrant(value); err != nil {
			return err
		}
	}
	return rowsError("processing consent grant", rows)
}

func validateProcessingIncarnations(ctx context.Context, tx metadataQuerier) error {
	incarnations, err := tx.QueryContext(ctx, `
		SELECT incarnation_id,created_at FROM processing_incarnations ORDER BY incarnation_id`)
	if err != nil {
		return fmt.Errorf("validating processing incarnations: %w", err)
	}
	defer func() { _ = incarnations.Close() }()
	for incarnations.Next() {
		value := metadataProcessingIncarnation{Type: metadataProcessingIncarnationType}
		if err := incarnations.Scan(&value.ID, &value.CreatedAt); err != nil {
			return err
		}
		if err := validateMetadataProcessingIncarnation(value); err != nil {
			return err
		}
	}
	return rowsError("processing incarnation", incarnations)
}

func validateCurrentRenditionRootState(ctx context.Context, tx metadataQuerier) (_ error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT root_id,root_kind,target_kind,target_id,fencing_token,recorded_at,
		       COALESCE(expires_at,''),active,released_at
		FROM current_rendition_roots ORDER BY root_id`)
	if err != nil {
		return fmt.Errorf("reading current rendition root state: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var root CurrentRenditionRoot
		var active bool
		var released sql.NullString
		if err := rows.Scan(&root.ID, &root.Kind, &root.TargetKind, &root.TargetID,
			&root.FencingToken, &root.RecordedAt, &root.ExpiresAt, &active, &released); err != nil {
			return fmt.Errorf("scanning current rendition root state: %w", err)
		}
		if err := validateCurrentRenditionRoot(root); err != nil {
			return fmt.Errorf("invalid current rendition root %s: %w", root.ID, err)
		}
		if active == released.Valid {
			return fmt.Errorf("current rendition root %s active state is inconsistent", root.ID)
		}
		if released.Valid {
			if err := validateMetadataTime("current rendition root released_at", released.String); err != nil {
				return err
			}
		}
		if !active {
			continue
		}
		var present bool
		switch root.TargetKind {
		case RenditionRootBuild:
			err = tx.QueryRowContext(ctx,
				`SELECT EXISTS(SELECT 1 FROM rendition_builds WHERE build_id=?)`, root.TargetID,
			).Scan(&present)
		case RenditionRootLexicalGeneration:
			var schema bool
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(
				SELECT 1 FROM sqlite_schema WHERE type='table'
				AND name='rendition_lexical_generations'
			)`).Scan(&schema)
			if err == nil && schema {
				err = tx.QueryRowContext(ctx, `SELECT EXISTS(
					SELECT 1 FROM rendition_lexical_generations WHERE generation_id=?
				)`, root.TargetID).Scan(&present)
			}
		}
		if err != nil {
			return fmt.Errorf("validating current rendition root %s target: %w", root.ID, err)
		}
		if !present {
			return fmt.Errorf("current rendition root %s target %s is missing", root.ID, root.TargetID)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return validateDerivativePurgeSuppressionState(ctx, tx)
}

func validateDerivativePurgeSuppressionState(
	ctx context.Context, tx metadataQuerier,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT source_sha256,profile_fingerprint,build_id,purged_at,active,
		       superseded_at,superseding_build_id
		FROM derivative_purge_suppressions
		ORDER BY source_sha256,profile_fingerprint,build_id`)
	if err != nil {
		return fmt.Errorf("reading derivative purge suppression state: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		value := metadataDerivativePurgeSuppression{Type: metadataDerivativePurgeSuppressionType}
		var supersededAt, supersedingBuildID sql.NullString
		if err := rows.Scan(&value.SourceSHA256, &value.ProfileFingerprint, &value.BuildID,
			&value.PurgedAt, &value.Active, &supersededAt, &supersedingBuildID); err != nil {
			return err
		}
		if supersededAt.Valid {
			value.SupersededAt = &supersededAt.String
		}
		if supersedingBuildID.Valid {
			value.SupersedingBuildID = &supersedingBuildID.String
		}
		if err := validateMetadataDerivativePurgeSuppression(value); err != nil {
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
