package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"golang.org/x/text/unicode/norm"
)

const (
	maxRenditionArtifacts          = 1024
	maxRenditionUnits              = 100_000
	maxRenditionLexicalSegments    = 1_000_000
	maxRenditionWarnings           = 256
	maxRenditionHeadingDepth       = 64
	maxCatalogIdentifierBytes      = 1 << 10
	maxCatalogTrustBoundaryBytes   = 1 << 10
	maxCatalogProviderOpBytes      = 4 << 10
	maxCatalogWarningBytes         = 4 << 10
	maxCatalogHeadingBytes         = 4 << 10
	maxCatalogLocatorJSONBytes     = 8 << 10
	maxCapturedArtifactPolicyBytes = 64 << 10
	maxProcessingProfileJSONBytes  = 1 << 20
	maxProviderReceiptJSONBytes    = 1 << 20
	maxLexicalSegmentRunes         = 1 << 20
	maxLexicalSegmentTextBytes     = 4 * maxLexicalSegmentRunes
	maxWarningsJSONBytes           = 2 << 20
	maxCatalogJSONBytes            = 2 << 20
	maxCatalogLocatorCoordinate    = int64(1_000_000_000_000_000)
)

const (
	catalogArtifactNormalizedEvidence = "normalized_evidence"
	catalogArtifactSanitizedMarkdown  = "sanitized_markdown"
)

type capturedArtifactPolicyV1 struct {
	Roles   []capturedArtifactRoleV1 `json:"roles"`
	Version int                      `json:"version"`
}

type capturedArtifactPolicyWireV1 struct {
	Roles   []jsontext.Value `json:"roles"`
	Version int              `json:"version"`
}

type capturedArtifactRoleV1 struct {
	MaxCount int    `json:"max_count"`
	MinCount int    `json:"min_count"`
	Role     string `json:"role"`
}

type normalizedCapturedArtifactPolicyV1 struct {
	canonical     jsontext.Value
	cardinalities map[string]capturedArtifactRoleV1
}

// RenditionArtifactState records whether retained derivative bytes were
// validated before their immutable build aggregate was staged.
type RenditionArtifactState string

const (
	// RenditionArtifactPending is never publishable and exists for callers to
	// describe an incomplete staging candidate.
	RenditionArtifactPending RenditionArtifactState = "pending"
	// RenditionArtifactVerified is the only state admitted to immutable
	// rendition build authority.
	RenditionArtifactVerified RenditionArtifactState = "verified"
)

// ProcessingProfileRecord is one canonical, non-secret processing policy and
// the component fingerprints derived by document.CanonicalProfile.
type ProcessingProfileRecord struct {
	Fingerprint                    string
	CanonicalProfile               jsontext.Value
	RenditionRequestFingerprint    string
	EvidenceLexicalFingerprint     string
	RetentionDisclosureFingerprint string
	AttachmentPolicyFingerprint    string
	ConsentFingerprint             string
	RenditionDisclosureFingerprint string
	TrustBoundary                  string
}

// RenditionArtifactRecord binds one verified derivative blob to a completed
// rendition build. Checksum identifies the exact logical blob bytes.
type RenditionArtifactRecord struct {
	ID       string
	Role     string
	BlobHash string
	Size     int64
	Checksum string
	State    RenditionArtifactState
}

// RenditionUnitRecord preserves one immutable normalized unit reference. Text
// payload authority remains in cataloged derivative blobs and lexical rows.
type RenditionUnitRecord struct {
	ID             string
	EvidenceUnitID string
	Order          int
	Checksum       string
	HeadingPath    []string
	Locator        document.EvidenceLocatorV1
}

// RenditionLexicalSegmentRecord is one model-independent lexical projection
// over a normalized rendition unit.
type RenditionLexicalSegmentRecord struct {
	ID        string
	UnitID    string
	Order     int
	CharStart int
	CharEnd   int
	Checksum  string
	Text      string
}

// RenditionBuildRecord is one completed, immutable vault-local rendition
// aggregate. Mutable attempts, leases, jobs, and embedding bindings are not
// represented here and therefore cannot affect build identity.
type RenditionBuildRecord struct {
	ID                                string
	VaultID                           string
	SourceSHA256                      string
	RenditionRequestFingerprint       string
	EvidenceLexicalFingerprint        string
	CapturedArtifactPolicyFingerprint string
	CapturedArtifactPolicy            jsontext.Value
	AuthorizationChecksum             string
	ProviderOperationID               string
	ProviderReceipt                   jsontext.Value
	EvidenceChecksum                  string
	RenditionChecksum                 string
	MarkdownChecksum                  string
	Completeness                      document.EvidenceCompleteness
	PartialSuccess                    bool
	Truncated                         bool
	Warnings                          []string
	CompletedAt                       string
	DeclaredArtifactCount             int
	Artifacts                         []RenditionArtifactRecord
	Units                             []RenditionUnitRecord
	LexicalSegments                   []RenditionLexicalSegmentRecord
}

// RenditionAttachmentRecord grants one content version authority to reuse a
// completed vault-local build under one complete processing profile identity.
type RenditionAttachmentRecord struct {
	ID               string
	VaultID          string
	ContentVersionID string
	BuildID          string
	Profile          ProcessingProfileRecord
	AttachedAt       string
}

// RenditionHeadRecord identifies the attachment visible at one exact content
// version and processing profile key.
type RenditionHeadRecord struct {
	ContentVersionID             string
	ProcessingProfileFingerprint string
	AttachmentID                 string
	PublishedAt                  string
}

// RenditionView is one active head and its exact immutable attachment/build
// authority, read from one SQLite snapshot.
type RenditionView struct {
	Head       RenditionHeadRecord
	Attachment RenditionAttachmentRecord
	Build      RenditionBuildRecord
}

// StageRenditionBuild inserts or exactly reuses one completed immutable build.
// No head can reach it until a separately authorized attachment is published.
func (s *Store) StageRenditionBuild(ctx context.Context, record RenditionBuildRecord) error {
	normalized, err := normalizeRenditionBuildRecord(record)
	if err != nil {
		return fmt.Errorf("staging rendition build: %w", err)
	}
	if normalized.VaultID != s.vaultID {
		return fmt.Errorf("staging rendition build: vault %q does not match store vault %q",
			normalized.VaultID, s.vaultID)
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := validateRenditionBuildBlobAuthorityTx(ctx, tx, normalized); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO rendition_builds(
				build_id,vault_uid,source_sha256,rendition_request_fingerprint,
				evidence_lexical_fingerprint,captured_artifact_policy_fingerprint,
				captured_artifact_policy_json,authorization_checksum,provider_operation_id,
				provider_receipt_json,evidence_checksum,rendition_checksum,markdown_checksum,
				completeness,partial_success,truncated,warnings_json,completed_at,
				declared_artifact_count,unit_count,lexical_segment_count
			) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			normalized.ID, normalized.VaultID, normalized.SourceSHA256,
			normalized.RenditionRequestFingerprint, normalized.EvidenceLexicalFingerprint,
			normalized.CapturedArtifactPolicyFingerprint, string(normalized.CapturedArtifactPolicy),
			normalized.AuthorizationChecksum, normalized.ProviderOperationID,
			string(normalized.ProviderReceipt), normalized.EvidenceChecksum,
			normalized.RenditionChecksum, normalized.MarkdownChecksum,
			normalized.Completeness, normalized.PartialSuccess, normalized.Truncated,
			mustCatalogJSON(normalized.Warnings), normalized.CompletedAt,
			normalized.DeclaredArtifactCount, len(normalized.Units), len(normalized.LexicalSegments),
		)
		if err != nil {
			return fmt.Errorf("inserting rendition build %s: %w", normalized.ID, err)
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("checking rendition build %s insertion: %w", normalized.ID, err)
		}
		if inserted == 0 {
			stored, loadErr := loadRenditionBuild(ctx, tx, normalized.ID)
			if errors.Is(loadErr, ErrNotFound) {
				var existingID string
				identityErr := tx.QueryRowContext(ctx, `
					SELECT build_id FROM rendition_builds
					WHERE vault_uid=? AND source_sha256=?
					  AND rendition_request_fingerprint=?
					  AND evidence_lexical_fingerprint=?
					  AND captured_artifact_policy_fingerprint=?`,
					normalized.VaultID, normalized.SourceSHA256,
					normalized.RenditionRequestFingerprint, normalized.EvidenceLexicalFingerprint,
					normalized.CapturedArtifactPolicyFingerprint,
				).Scan(&existingID)
				if identityErr == nil {
					return fmt.Errorf("rendition build identity already belongs to immutable build %s", existingID)
				}
			}
			if loadErr != nil {
				return loadErr
			}
			if !reflect.DeepEqual(stored, normalized) {
				return fmt.Errorf("rendition build %s names different immutable metadata", normalized.ID)
			}
			return validateRenditionBuildStateTx(ctx, tx, normalized.ID)
		}
		if err := insertRenditionBuildChildrenTx(ctx, tx, normalized); err != nil {
			return err
		}
		return validateRenditionBuildStateTx(ctx, tx, normalized.ID)
	})
}

// AttachRenditionBuild inserts or exactly reuses one version-scoped authority
// grant. Sharing bytes never carries another version's profile or consent.
func (s *Store) AttachRenditionBuild(ctx context.Context, record RenditionAttachmentRecord) error {
	normalized, err := normalizeRenditionAttachmentRecord(record)
	if err != nil {
		return fmt.Errorf("attaching rendition build: %w", err)
	}
	if normalized.VaultID != s.vaultID {
		return fmt.Errorf("attaching rendition build: vault %q does not match store vault %q",
			normalized.VaultID, s.vaultID)
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if err := ensureProcessingProfileTx(ctx, tx, normalized.Profile); err != nil {
			return err
		}
		build, err := loadRenditionBuild(ctx, tx, normalized.BuildID)
		if err != nil {
			return fmt.Errorf("reading rendition build %s: %w", normalized.BuildID, err)
		}
		if build.VaultID != normalized.VaultID {
			return errors.New("rendition attachment and build belong to different vaults")
		}
		if build.RenditionRequestFingerprint != normalized.Profile.RenditionRequestFingerprint ||
			build.EvidenceLexicalFingerprint != normalized.Profile.EvidenceLexicalFingerprint {
			return errors.New("rendition attachment profile does not match build component identity")
		}
		var sourceSHA256 string
		if err := tx.QueryRowContext(ctx,
			`SELECT blob_hash FROM content_versions WHERE version_id=?`, normalized.ContentVersionID,
		).Scan(&sourceSHA256); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("content version %s: %w", normalized.ContentVersionID, ErrNotFound)
		} else if err != nil {
			return fmt.Errorf("reading content version %s: %w", normalized.ContentVersionID, err)
		}
		if sourceSHA256 != build.SourceSHA256 {
			return errors.New("rendition attachment source does not match content version")
		}
		if err := validateRenditionBuildStateTx(ctx, tx, build.ID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO rendition_attachments(
				attachment_id,vault_uid,content_version_id,build_id,profile_fingerprint,
				retention_disclosure_fingerprint,attachment_policy_fingerprint,
				consent_fingerprint,rendition_disclosure_fingerprint,trust_boundary,attached_at
			) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			normalized.ID, normalized.VaultID, normalized.ContentVersionID, normalized.BuildID,
			normalized.Profile.Fingerprint, normalized.Profile.RetentionDisclosureFingerprint,
			normalized.Profile.AttachmentPolicyFingerprint, normalized.Profile.ConsentFingerprint,
			normalized.Profile.RenditionDisclosureFingerprint, normalized.Profile.TrustBoundary,
			normalized.AttachedAt,
		)
		if err != nil {
			return fmt.Errorf("inserting rendition attachment %s: %w", normalized.ID, err)
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("checking rendition attachment %s insertion: %w", normalized.ID, err)
		}
		if inserted != 0 {
			return nil
		}
		stored, err := loadRenditionAttachment(ctx, tx, normalized.ID)
		if err != nil {
			return fmt.Errorf("reading rendition attachment %s: %w", normalized.ID, err)
		}
		if !reflect.DeepEqual(stored, normalized) {
			return fmt.Errorf("rendition attachment %s names different immutable metadata", normalized.ID)
		}
		return nil
	})
}

// PublishRenditionHead atomically validates and activates one exact
// version/profile attachment. Any failure leaves the prior head unchanged.
func (s *Store) PublishRenditionHead(ctx context.Context, record RenditionHeadRecord) error {
	if err := validateRenditionHeadRecord(record); err != nil {
		return fmt.Errorf("publishing rendition head: %w", err)
	}
	return s.withStorageTx(ctx, func(tx *sql.Tx) error {
		attachment, err := loadRenditionAttachment(ctx, tx, record.AttachmentID)
		if err != nil {
			return fmt.Errorf("reading rendition head attachment %s: %w", record.AttachmentID, err)
		}
		if attachment.ContentVersionID != record.ContentVersionID ||
			attachment.Profile.Fingerprint != record.ProcessingProfileFingerprint {
			return errors.New("rendition head does not resolve through its exact attachment")
		}
		if err := validateRenditionBuildStateTx(ctx, tx, attachment.BuildID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO rendition_heads(
				content_version_id,profile_fingerprint,attachment_id,published_at
			) VALUES(?,?,?,?)
			ON CONFLICT(content_version_id,profile_fingerprint) DO UPDATE SET
				attachment_id=excluded.attachment_id,
				published_at=excluded.published_at`,
			record.ContentVersionID, record.ProcessingProfileFingerprint,
			record.AttachmentID, record.PublishedAt,
		)
		if err != nil {
			return fmt.Errorf("publishing rendition head: %w", err)
		}
		return nil
	})
}

// ActiveRendition returns the active attachment and immutable build at one
// exact content-version/profile key. It does not alter legacy text serving.
func (s *Store) ActiveRendition(
	ctx context.Context, contentVersionID, processingProfileFingerprint string,
) (RenditionView, error) {
	if err := validateUUIDv4(contentVersionID); err != nil {
		return RenditionView{}, fmt.Errorf("active rendition content version %q: %w", contentVersionID, ErrNotFound)
	}
	if err := validateCatalogSHA256(processingProfileFingerprint, "processing profile fingerprint"); err != nil {
		return RenditionView{}, fmt.Errorf("active rendition profile %q: %w", processingProfileFingerprint, ErrNotFound)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return RenditionView{}, fmt.Errorf("starting active rendition snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	view := RenditionView{Head: RenditionHeadRecord{
		ContentVersionID: contentVersionID, ProcessingProfileFingerprint: processingProfileFingerprint,
	}}
	err = tx.QueryRowContext(ctx, `
		SELECT attachment_id,published_at FROM rendition_heads
		WHERE content_version_id=? AND profile_fingerprint=?`,
		contentVersionID, processingProfileFingerprint,
	).Scan(&view.Head.AttachmentID, &view.Head.PublishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RenditionView{}, ErrNotFound
	}
	if err != nil {
		return RenditionView{}, fmt.Errorf("reading active rendition head: %w", err)
	}
	view.Attachment, err = loadRenditionAttachment(ctx, tx, view.Head.AttachmentID)
	if err != nil {
		return RenditionView{}, fmt.Errorf("reading active rendition attachment: %w", err)
	}
	view.Build, err = loadRenditionBuild(ctx, tx, view.Attachment.BuildID)
	if err != nil {
		return RenditionView{}, fmt.Errorf("reading active rendition build: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return RenditionView{}, fmt.Errorf("closing active rendition snapshot: %w", err)
	}
	return view, nil
}

func normalizeProcessingProfileRecord(record ProcessingProfileRecord) (ProcessingProfileRecord, error) {
	canonical, err := canonicalCatalogJSON(record.CanonicalProfile, "processing profile")
	if err != nil {
		return ProcessingProfileRecord{}, err
	}
	if len(canonical) > maxProcessingProfileJSONBytes {
		return ProcessingProfileRecord{}, fmt.Errorf(
			"processing profile JSON exceeds %d bytes", maxProcessingProfileJSONBytes,
		)
	}
	var profile document.ProcessingProfileV1
	if err := json.Unmarshal(canonical, &profile, json.RejectUnknownMembers(true)); err != nil {
		return ProcessingProfileRecord{}, fmt.Errorf("decoding processing profile: %w", err)
	}
	wantCanonical, fingerprints, err := document.CanonicalProfile(profile)
	if err != nil {
		return ProcessingProfileRecord{}, err
	}
	if !bytes.Equal(canonical, wantCanonical) {
		return ProcessingProfileRecord{}, errors.New("processing profile JSON is not canonical")
	}
	if profile.Rendition == nil {
		return ProcessingProfileRecord{}, errors.New("rendition attachment profile lacks a rendition binding")
	}
	checks := []struct {
		name string
		got  string
		want string
	}{
		{"profile fingerprint", record.Fingerprint, fingerprints.Profile},
		{"rendition request fingerprint", record.RenditionRequestFingerprint, fingerprints.RenditionRequest},
		{"evidence lexical fingerprint", record.EvidenceLexicalFingerprint, fingerprints.EvidenceLexical},
		{"retention disclosure fingerprint", record.RetentionDisclosureFingerprint, fingerprints.RetentionDisclosure},
		{"attachment policy fingerprint", record.AttachmentPolicyFingerprint, profile.RetentionDisclosure.AttachmentPolicyFingerprint},
		{"consent fingerprint", record.ConsentFingerprint, profile.RetentionDisclosure.ConsentFingerprint},
		{"rendition disclosure fingerprint", record.RenditionDisclosureFingerprint, profile.Rendition.DisclosureFingerprint},
		{"trust boundary", record.TrustBoundary, profile.RetentionDisclosure.TrustBoundary},
	}
	for _, check := range checks {
		if check.got != check.want {
			return ProcessingProfileRecord{}, fmt.Errorf("%s does not match canonical profile", check.name)
		}
	}
	if err := validateCatalogUTF8(record.TrustBoundary, maxCatalogTrustBoundaryBytes, "trust boundary", false); err != nil {
		return ProcessingProfileRecord{}, err
	}
	record.CanonicalProfile = canonical
	return record, nil
}

func normalizeRenditionBuildRecord(record RenditionBuildRecord) (RenditionBuildRecord, error) {
	if len(record.Artifacts) > maxRenditionArtifacts {
		return RenditionBuildRecord{}, fmt.Errorf("rendition build has more than %d artifacts", maxRenditionArtifacts)
	}
	if len(record.Units) > maxRenditionUnits {
		return RenditionBuildRecord{}, fmt.Errorf("rendition build has more than %d units", maxRenditionUnits)
	}
	if len(record.LexicalSegments) > maxRenditionLexicalSegments {
		return RenditionBuildRecord{}, fmt.Errorf(
			"rendition build has more than %d lexical segments", maxRenditionLexicalSegments,
		)
	}
	for name, value := range map[string]string{
		"build ID": record.ID, "source SHA-256": record.SourceSHA256,
		"rendition request fingerprint":        record.RenditionRequestFingerprint,
		"evidence lexical fingerprint":         record.EvidenceLexicalFingerprint,
		"captured artifact policy fingerprint": record.CapturedArtifactPolicyFingerprint,
		"authorization checksum":               record.AuthorizationChecksum,
		"evidence checksum":                    record.EvidenceChecksum, "rendition checksum": record.RenditionChecksum,
		"Markdown checksum": record.MarkdownChecksum,
	} {
		if err := validateCatalogSHA256(value, name); err != nil {
			return RenditionBuildRecord{}, err
		}
	}
	if err := validateUUIDv4(record.VaultID); err != nil {
		return RenditionBuildRecord{}, fmt.Errorf("invalid build vault ID: %w", err)
	}
	if err := validateCatalogUTF8(
		record.ProviderOperationID, maxCatalogProviderOpBytes, "provider operation ID", false,
	); err != nil {
		return RenditionBuildRecord{}, err
	}
	if err := validateMetadataTime("rendition build completed_at", record.CompletedAt); err != nil {
		return RenditionBuildRecord{}, err
	}
	switch record.Completeness {
	case document.EvidenceComplete, document.EvidencePartial, document.EvidenceDegradedProvenance:
	default:
		return RenditionBuildRecord{}, fmt.Errorf("invalid rendition completeness %q", record.Completeness)
	}
	capturedPolicy, err := normalizeCapturedArtifactPolicyV1(record.CapturedArtifactPolicy)
	if err != nil {
		return RenditionBuildRecord{}, err
	}
	if digestCatalogJSON(capturedPolicy.canonical) != record.CapturedArtifactPolicyFingerprint {
		return RenditionBuildRecord{}, errors.New("captured artifact policy fingerprint does not match policy JSON")
	}
	receipt, err := canonicalCatalogJSON(record.ProviderReceipt, "provider receipt")
	if err != nil {
		return RenditionBuildRecord{}, err
	}
	if len(receipt) > maxProviderReceiptJSONBytes {
		return RenditionBuildRecord{}, fmt.Errorf(
			"provider receipt JSON exceeds %d bytes", maxProviderReceiptJSONBytes,
		)
	}
	if record.Warnings == nil {
		record.Warnings = make([]string, 0)
	} else {
		record.Warnings = append([]string(nil), record.Warnings...)
	}
	if len(record.Warnings) > maxRenditionWarnings {
		return RenditionBuildRecord{}, fmt.Errorf("rendition build has more than %d warnings", maxRenditionWarnings)
	}
	for _, warning := range record.Warnings {
		if err := validateCatalogUTF8(warning, maxCatalogWarningBytes, "rendition warning", true); err != nil {
			return RenditionBuildRecord{}, err
		}
	}
	warningsJSON, err := json.Marshal(record.Warnings, json.Deterministic(true))
	if err != nil {
		return RenditionBuildRecord{}, fmt.Errorf("encoding rendition warnings: %w", err)
	}
	if len(warningsJSON) > maxWarningsJSONBytes {
		return RenditionBuildRecord{}, fmt.Errorf("rendition warnings JSON exceeds %d bytes", maxWarningsJSONBytes)
	}
	record.CapturedArtifactPolicy = capturedPolicy.canonical
	record.ProviderReceipt = receipt
	record.Artifacts = append([]RenditionArtifactRecord(nil), record.Artifacts...)
	sort.Slice(record.Artifacts, func(i, j int) bool {
		return record.Artifacts[i].ID < record.Artifacts[j].ID
	})
	record.Units = append([]RenditionUnitRecord(nil), record.Units...)
	record.LexicalSegments = append([]RenditionLexicalSegmentRecord(nil), record.LexicalSegments...)
	if record.DeclaredArtifactCount < 0 || record.DeclaredArtifactCount > maxRenditionArtifacts {
		return RenditionBuildRecord{}, fmt.Errorf(
			"declared artifact count must be between 0 and %d", maxRenditionArtifacts,
		)
	}
	if record.DeclaredArtifactCount != len(record.Artifacts) {
		return RenditionBuildRecord{}, fmt.Errorf("declared artifact count %d does not match %d artifact rows",
			record.DeclaredArtifactCount, len(record.Artifacts))
	}
	seenArtifacts := make(map[string]bool, len(record.Artifacts))
	artifactRoleCounts := make(map[string]int, len(capturedPolicy.cardinalities))
	for index, artifact := range record.Artifacts {
		if err := validateCatalogUTF8(
			artifact.ID, maxCatalogIdentifierBytes, fmt.Sprintf("rendition artifact %d ID", index), false,
		); err != nil {
			return RenditionBuildRecord{}, err
		}
		if err := validateCatalogUTF8(artifact.Role, 64, fmt.Sprintf("rendition artifact %d role", index), false); err != nil {
			return RenditionBuildRecord{}, err
		}
		if seenArtifacts[artifact.ID] {
			return RenditionBuildRecord{}, fmt.Errorf("rendition artifact %q is duplicated", artifact.ID)
		}
		seenArtifacts[artifact.ID] = true
		cardinality, declared := capturedPolicy.cardinalities[artifact.Role]
		if !declared {
			return RenditionBuildRecord{}, fmt.Errorf(
				"rendition artifact role %q is not declared by captured policy", artifact.Role,
			)
		}
		artifactRoleCounts[artifact.Role]++
		if artifactRoleCounts[artifact.Role] > cardinality.MaxCount {
			return RenditionBuildRecord{}, fmt.Errorf(
				"rendition artifact role %q exceeds captured policy maximum %d",
				artifact.Role, cardinality.MaxCount,
			)
		}
		if artifact.Size < 0 {
			return RenditionBuildRecord{}, fmt.Errorf("rendition artifact %q has negative size", artifact.ID)
		}
		if err := validateCatalogSHA256(artifact.BlobHash, "rendition artifact blob hash"); err != nil {
			return RenditionBuildRecord{}, err
		}
		if artifact.Checksum != artifact.BlobHash {
			return RenditionBuildRecord{}, fmt.Errorf("rendition artifact %q checksum disagrees with blob hash", artifact.ID)
		}
		if artifact.State != RenditionArtifactVerified {
			return RenditionBuildRecord{}, fmt.Errorf("rendition artifact %q is not verified", artifact.ID)
		}
	}
	for role, cardinality := range capturedPolicy.cardinalities {
		count := artifactRoleCounts[role]
		if count < cardinality.MinCount || count > cardinality.MaxCount {
			return RenditionBuildRecord{}, fmt.Errorf(
				"captured artifact role %q count %d is outside [%d,%d]",
				role, count, cardinality.MinCount, cardinality.MaxCount,
			)
		}
	}
	seenUnits := make(map[string]bool, len(record.Units))
	for index := range record.Units {
		unit := &record.Units[index]
		unit.HeadingPath = append([]string(nil), unit.HeadingPath...)
		if unit.Order != index {
			return RenditionBuildRecord{}, fmt.Errorf("rendition unit %d is not in canonical order", index)
		}
		if err := validateCatalogUTF8(unit.ID, maxCatalogIdentifierBytes, "rendition unit ID", false); err != nil {
			return RenditionBuildRecord{}, err
		}
		if err := validateCatalogUTF8(
			unit.EvidenceUnitID, maxCatalogIdentifierBytes, "rendition evidence unit ID", false,
		); err != nil {
			return RenditionBuildRecord{}, err
		}
		if err := validateCatalogSHA256(unit.Checksum, "rendition unit checksum"); err != nil {
			return RenditionBuildRecord{}, err
		}
		if seenUnits[unit.ID] {
			return RenditionBuildRecord{}, fmt.Errorf("rendition unit %q is duplicated", unit.ID)
		}
		seenUnits[unit.ID] = true
		if len(unit.HeadingPath) > maxRenditionHeadingDepth {
			return RenditionBuildRecord{}, fmt.Errorf(
				"rendition unit %q has more than %d headings", unit.ID, maxRenditionHeadingDepth,
			)
		}
		for _, heading := range unit.HeadingPath {
			if err := validateCatalogHeading(heading); err != nil {
				return RenditionBuildRecord{}, err
			}
		}
		if err := validateCatalogLocatorV1(unit.Locator); err != nil {
			return RenditionBuildRecord{}, fmt.Errorf("rendition unit %q locator: %w", unit.ID, err)
		}
		locatorJSON, err := json.Marshal(unit.Locator, json.Deterministic(true))
		if err != nil {
			return RenditionBuildRecord{}, fmt.Errorf("encoding rendition locator: %w", err)
		}
		if len(locatorJSON) > maxCatalogLocatorJSONBytes {
			return RenditionBuildRecord{}, fmt.Errorf("rendition locator JSON exceeds %d bytes", maxCatalogLocatorJSONBytes)
		}
	}
	seenSegments := make(map[string]bool, len(record.LexicalSegments))
	for index, segment := range record.LexicalSegments {
		if segment.Order != index || segment.CharStart < 0 ||
			segment.CharEnd < segment.CharStart || segment.CharEnd-segment.CharStart != utf8.RuneCountInString(segment.Text) {
			return RenditionBuildRecord{}, fmt.Errorf("rendition lexical segment %d is invalid", index)
		}
		if err := validateCatalogUTF8(segment.ID, maxCatalogIdentifierBytes, "rendition lexical segment ID", false); err != nil {
			return RenditionBuildRecord{}, err
		}
		if err := validateCatalogUTF8(segment.UnitID, maxCatalogIdentifierBytes, "rendition lexical unit ID", false); err != nil {
			return RenditionBuildRecord{}, err
		}
		if err := validateCatalogUTF8(
			segment.Text, maxLexicalSegmentTextBytes, "rendition lexical segment text", true,
		); err != nil {
			return RenditionBuildRecord{}, err
		}
		if utf8.RuneCountInString(segment.Text) > maxLexicalSegmentRunes {
			return RenditionBuildRecord{}, fmt.Errorf(
				"rendition lexical segment text exceeds %d runes", maxLexicalSegmentRunes,
			)
		}
		if !seenUnits[segment.UnitID] {
			return RenditionBuildRecord{}, fmt.Errorf("rendition lexical segment %q references missing unit", segment.ID)
		}
		if seenSegments[segment.ID] {
			return RenditionBuildRecord{}, fmt.Errorf("rendition lexical segment %q is duplicated", segment.ID)
		}
		seenSegments[segment.ID] = true
		if err := validateCatalogSHA256(segment.Checksum, "rendition lexical segment checksum"); err != nil {
			return RenditionBuildRecord{}, err
		}
	}
	return record, nil
}

func normalizeRenditionAttachmentRecord(record RenditionAttachmentRecord) (RenditionAttachmentRecord, error) {
	for name, value := range map[string]string{"attachment ID": record.ID, "build ID": record.BuildID} {
		if err := validateCatalogSHA256(value, name); err != nil {
			return RenditionAttachmentRecord{}, err
		}
	}
	if err := validateUUIDv4(record.VaultID); err != nil {
		return RenditionAttachmentRecord{}, fmt.Errorf("invalid attachment vault ID: %w", err)
	}
	if err := validateUUIDv4(record.ContentVersionID); err != nil {
		return RenditionAttachmentRecord{}, fmt.Errorf("invalid attachment content version ID: %w", err)
	}
	if err := validateMetadataTime("rendition attachment attached_at", record.AttachedAt); err != nil {
		return RenditionAttachmentRecord{}, err
	}
	profile, err := normalizeProcessingProfileRecord(record.Profile)
	if err != nil {
		return RenditionAttachmentRecord{}, err
	}
	record.Profile = profile
	return record, nil
}

func validateRenditionHeadRecord(record RenditionHeadRecord) error {
	if err := validateUUIDv4(record.ContentVersionID); err != nil {
		return fmt.Errorf("invalid head content version ID: %w", err)
	}
	if err := validateCatalogSHA256(record.ProcessingProfileFingerprint, "head processing profile fingerprint"); err != nil {
		return err
	}
	if err := validateCatalogSHA256(record.AttachmentID, "head attachment ID"); err != nil {
		return err
	}
	return validateMetadataTime("rendition head published_at", record.PublishedAt)
}

func ensureProcessingProfileTx(ctx context.Context, tx *sql.Tx, record ProcessingProfileRecord) error {
	result, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO processing_profiles(
			profile_fingerprint,canonical_profile,rendition_request_fingerprint,
			evidence_lexical_fingerprint,retention_disclosure_fingerprint,
			attachment_policy_fingerprint,consent_fingerprint,
			rendition_disclosure_fingerprint,trust_boundary
		) VALUES(?,?,?,?,?,?,?,?,?)`,
		record.Fingerprint, string(record.CanonicalProfile), record.RenditionRequestFingerprint,
		record.EvidenceLexicalFingerprint, record.RetentionDisclosureFingerprint,
		record.AttachmentPolicyFingerprint, record.ConsentFingerprint,
		record.RenditionDisclosureFingerprint, record.TrustBoundary,
	)
	if err != nil {
		return fmt.Errorf("inserting processing profile %s: %w", record.Fingerprint, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking processing profile %s insertion: %w", record.Fingerprint, err)
	}
	if inserted != 0 {
		return nil
	}
	stored, err := loadProcessingProfile(ctx, tx, record.Fingerprint)
	if err != nil {
		return fmt.Errorf("reading processing profile %s: %w", record.Fingerprint, err)
	}
	if !reflect.DeepEqual(stored, record) {
		return fmt.Errorf("processing profile %s names different immutable metadata", record.Fingerprint)
	}
	return nil
}

func insertRenditionBuildChildrenTx(ctx context.Context, tx *sql.Tx, record RenditionBuildRecord) error {
	for _, artifact := range record.Artifacts {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_artifacts(build_id,artifact_id,role,blob_hash,size,checksum,state)
			VALUES(?,?,?,?,?,?,?)`, record.ID, artifact.ID, artifact.Role, artifact.BlobHash,
			artifact.Size, artifact.Checksum, artifact.State); err != nil {
			return fmt.Errorf("inserting rendition artifact %s: %w", artifact.ID, err)
		}
	}
	for _, unit := range record.Units {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_units(
				build_id,unit_id,evidence_unit_id,unit_order,checksum,heading_path_json,locator_json
			) VALUES(?,?,?,?,?,?,?)`, record.ID, unit.ID, unit.EvidenceUnitID, unit.Order,
			unit.Checksum, mustCatalogJSON(unit.HeadingPath), mustCatalogJSON(unit.Locator)); err != nil {
			return fmt.Errorf("inserting rendition unit %s: %w", unit.ID, err)
		}
	}
	for _, segment := range record.LexicalSegments {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO rendition_lexical_segments(
				build_id,segment_id,unit_id,segment_order,char_start,char_end,checksum,text
			) VALUES(?,?,?,?,?,?,?,?)`, record.ID, segment.ID, segment.UnitID, segment.Order,
			segment.CharStart, segment.CharEnd, segment.Checksum, segment.Text); err != nil {
			return fmt.Errorf("inserting rendition lexical segment %s: %w", segment.ID, err)
		}
	}
	return nil
}

func validateRenditionBuildBlobAuthorityTx(
	ctx context.Context, tx *sql.Tx, record RenditionBuildRecord,
) error {
	var sourceSize int64
	if err := tx.QueryRowContext(ctx, `SELECT size FROM blobs WHERE hash=?`, record.SourceSHA256).Scan(&sourceSize); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("rendition source blob %s: %w", record.SourceSHA256, ErrNotFound)
	} else if err != nil {
		return fmt.Errorf("reading rendition source blob %s: %w", record.SourceSHA256, err)
	}
	if _, err := requirePhysicalAuthorityTx(tx, record.SourceSHA256); err != nil {
		return fmt.Errorf("validating rendition source blob %s: %w", record.SourceSHA256, err)
	}
	for _, artifact := range record.Artifacts {
		var size int64
		if err := tx.QueryRowContext(ctx, `SELECT size FROM blobs WHERE hash=?`, artifact.BlobHash).Scan(&size); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("rendition artifact blob %s: %w", artifact.BlobHash, ErrNotFound)
		} else if err != nil {
			return fmt.Errorf("reading rendition artifact blob %s: %w", artifact.BlobHash, err)
		}
		if size != artifact.Size {
			return fmt.Errorf("rendition artifact %s size %d disagrees with blob size %d", artifact.ID, artifact.Size, size)
		}
		if _, err := requirePhysicalAuthorityTx(tx, artifact.BlobHash); err != nil {
			return fmt.Errorf("validating rendition artifact blob %s: %w", artifact.BlobHash, err)
		}
	}
	return nil
}

func validateRenditionBuildStateTx(ctx context.Context, tx metadataQuerier, buildID string) error {
	var declaredArtifacts, declaredUnits, declaredSegments int
	var artifacts, verifiedArtifacts, units, segments int
	err := tx.QueryRowContext(ctx, `
		SELECT b.declared_artifact_count,b.unit_count,b.lexical_segment_count,
		       (SELECT COUNT(*) FROM rendition_artifacts a WHERE a.build_id=b.build_id),
		       (SELECT COUNT(*) FROM rendition_artifacts a
		          JOIN blobs blob ON blob.hash=a.blob_hash AND blob.size=a.size
		        WHERE a.build_id=b.build_id AND a.state='verified' AND a.checksum=a.blob_hash),
		       (SELECT COUNT(*) FROM rendition_units u WHERE u.build_id=b.build_id),
		       (SELECT COUNT(*) FROM rendition_lexical_segments l WHERE l.build_id=b.build_id)
		FROM rendition_builds b WHERE b.build_id=?`, buildID,
	).Scan(&declaredArtifacts, &declaredUnits, &declaredSegments,
		&artifacts, &verifiedArtifacts, &units, &segments)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("rendition build %s: %w", buildID, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("validating rendition build %s: %w", buildID, err)
	}
	if artifacts != declaredArtifacts || verifiedArtifacts != declaredArtifacts {
		return fmt.Errorf("rendition build %s artifact membership is incomplete or unverified", buildID)
	}
	if units != declaredUnits {
		return fmt.Errorf("rendition build %s unit membership is incomplete", buildID)
	}
	if segments != declaredSegments {
		return fmt.Errorf("rendition build %s lexical membership is incomplete", buildID)
	}
	return nil
}

func loadProcessingProfile(ctx context.Context, tx metadataQuerier, fingerprint string) (ProcessingProfileRecord, error) {
	record := ProcessingProfileRecord{Fingerprint: fingerprint}
	var canonical string
	err := tx.QueryRowContext(ctx, `
		SELECT canonical_profile,rendition_request_fingerprint,evidence_lexical_fingerprint,
		       retention_disclosure_fingerprint,attachment_policy_fingerprint,
		       consent_fingerprint,rendition_disclosure_fingerprint,trust_boundary
		FROM processing_profiles WHERE profile_fingerprint=?`, fingerprint,
	).Scan(&canonical, &record.RenditionRequestFingerprint, &record.EvidenceLexicalFingerprint,
		&record.RetentionDisclosureFingerprint, &record.AttachmentPolicyFingerprint,
		&record.ConsentFingerprint, &record.RenditionDisclosureFingerprint, &record.TrustBoundary)
	if errors.Is(err, sql.ErrNoRows) {
		return ProcessingProfileRecord{}, ErrNotFound
	}
	if err != nil {
		return ProcessingProfileRecord{}, err
	}
	record.CanonicalProfile = jsontext.Value(canonical)
	return record, nil
}

func loadRenditionAttachment(ctx context.Context, tx metadataQuerier, attachmentID string) (RenditionAttachmentRecord, error) {
	record := RenditionAttachmentRecord{ID: attachmentID}
	var profileFingerprint string
	err := tx.QueryRowContext(ctx, `
		SELECT vault_uid,content_version_id,build_id,profile_fingerprint,attached_at
		FROM rendition_attachments WHERE attachment_id=?`, attachmentID,
	).Scan(&record.VaultID, &record.ContentVersionID, &record.BuildID, &profileFingerprint, &record.AttachedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RenditionAttachmentRecord{}, ErrNotFound
	}
	if err != nil {
		return RenditionAttachmentRecord{}, err
	}
	record.Profile, err = loadProcessingProfile(ctx, tx, profileFingerprint)
	return record, err
}

func loadRenditionBuild(ctx context.Context, tx metadataQuerier, buildID string) (RenditionBuildRecord, error) {
	record := RenditionBuildRecord{ID: buildID}
	var policy, receipt, warnings string
	var partialSuccess, truncated int
	var unitCount, segmentCount int
	err := tx.QueryRowContext(ctx, `
		SELECT vault_uid,source_sha256,rendition_request_fingerprint,
		       evidence_lexical_fingerprint,captured_artifact_policy_fingerprint,
		       captured_artifact_policy_json,authorization_checksum,provider_operation_id,
		       provider_receipt_json,evidence_checksum,rendition_checksum,markdown_checksum,
		       completeness,partial_success,truncated,warnings_json,completed_at,
		       declared_artifact_count,unit_count,lexical_segment_count
		FROM rendition_builds WHERE build_id=?`, buildID,
	).Scan(&record.VaultID, &record.SourceSHA256, &record.RenditionRequestFingerprint,
		&record.EvidenceLexicalFingerprint, &record.CapturedArtifactPolicyFingerprint,
		&policy, &record.AuthorizationChecksum, &record.ProviderOperationID, &receipt,
		&record.EvidenceChecksum, &record.RenditionChecksum, &record.MarkdownChecksum,
		&record.Completeness, &partialSuccess, &truncated, &warnings, &record.CompletedAt,
		&record.DeclaredArtifactCount, &unitCount, &segmentCount)
	if errors.Is(err, sql.ErrNoRows) {
		return RenditionBuildRecord{}, ErrNotFound
	}
	if err != nil {
		return RenditionBuildRecord{}, err
	}
	if record.DeclaredArtifactCount < 0 || record.DeclaredArtifactCount > maxRenditionArtifacts ||
		unitCount < 0 || unitCount > maxRenditionUnits ||
		segmentCount < 0 || segmentCount > maxRenditionLexicalSegments {
		return RenditionBuildRecord{}, fmt.Errorf("rendition build %s has out-of-range child counts", buildID)
	}
	record.CapturedArtifactPolicy = jsontext.Value(policy)
	record.ProviderReceipt = jsontext.Value(receipt)
	record.PartialSuccess = partialSuccess != 0
	record.Truncated = truncated != 0
	if err := json.Unmarshal([]byte(warnings), &record.Warnings); err != nil {
		return RenditionBuildRecord{}, fmt.Errorf("decoding rendition warnings: %w", err)
	}
	record.Artifacts = make([]RenditionArtifactRecord, 0, record.DeclaredArtifactCount)
	rows, err := tx.QueryContext(ctx, `
		SELECT artifact_id,role,blob_hash,size,checksum,state
		FROM rendition_artifacts WHERE build_id=? ORDER BY artifact_id`, buildID)
	if err != nil {
		return RenditionBuildRecord{}, err
	}
	for rows.Next() {
		if len(record.Artifacts) == maxRenditionArtifacts {
			_ = rows.Close()
			return RenditionBuildRecord{}, fmt.Errorf("rendition build %s exceeds artifact limit", buildID)
		}
		var artifact RenditionArtifactRecord
		if err := rows.Scan(&artifact.ID, &artifact.Role, &artifact.BlobHash,
			&artifact.Size, &artifact.Checksum, &artifact.State); err != nil {
			_ = rows.Close()
			return RenditionBuildRecord{}, err
		}
		record.Artifacts = append(record.Artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return RenditionBuildRecord{}, err
	}
	if err := rows.Close(); err != nil {
		return RenditionBuildRecord{}, err
	}
	record.Units = make([]RenditionUnitRecord, 0, unitCount)
	rows, err = tx.QueryContext(ctx, `
		SELECT unit_id,evidence_unit_id,unit_order,checksum,heading_path_json,locator_json
		FROM rendition_units WHERE build_id=? ORDER BY unit_order,unit_id`, buildID)
	if err != nil {
		return RenditionBuildRecord{}, err
	}
	for rows.Next() {
		if len(record.Units) == maxRenditionUnits {
			_ = rows.Close()
			return RenditionBuildRecord{}, fmt.Errorf("rendition build %s exceeds unit limit", buildID)
		}
		var unit RenditionUnitRecord
		var headingPath, locator string
		if err := rows.Scan(&unit.ID, &unit.EvidenceUnitID, &unit.Order, &unit.Checksum,
			&headingPath, &locator); err != nil {
			_ = rows.Close()
			return RenditionBuildRecord{}, err
		}
		if err := json.Unmarshal([]byte(headingPath), &unit.HeadingPath); err != nil {
			_ = rows.Close()
			return RenditionBuildRecord{}, err
		}
		if err := json.Unmarshal([]byte(locator), &unit.Locator); err != nil {
			_ = rows.Close()
			return RenditionBuildRecord{}, err
		}
		record.Units = append(record.Units, unit)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return RenditionBuildRecord{}, err
	}
	if err := rows.Close(); err != nil {
		return RenditionBuildRecord{}, err
	}
	record.LexicalSegments = make([]RenditionLexicalSegmentRecord, 0, segmentCount)
	rows, err = tx.QueryContext(ctx, `
		SELECT segment_id,unit_id,segment_order,char_start,char_end,checksum,text
		FROM rendition_lexical_segments WHERE build_id=? ORDER BY segment_order,segment_id`, buildID)
	if err != nil {
		return RenditionBuildRecord{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if len(record.LexicalSegments) == maxRenditionLexicalSegments {
			return RenditionBuildRecord{}, fmt.Errorf("rendition build %s exceeds lexical segment limit", buildID)
		}
		var segment RenditionLexicalSegmentRecord
		if err := rows.Scan(&segment.ID, &segment.UnitID, &segment.Order, &segment.CharStart,
			&segment.CharEnd, &segment.Checksum, &segment.Text); err != nil {
			return RenditionBuildRecord{}, err
		}
		record.LexicalSegments = append(record.LexicalSegments, segment)
	}
	if err := rows.Err(); err != nil {
		return RenditionBuildRecord{}, err
	}
	return record, nil
}

func normalizeCapturedArtifactPolicyV1(raw jsontext.Value) (normalizedCapturedArtifactPolicyV1, error) {
	canonical, err := canonicalCatalogJSON(raw, "captured artifact policy")
	if err != nil {
		return normalizedCapturedArtifactPolicyV1{}, err
	}
	if len(canonical) > maxCapturedArtifactPolicyBytes {
		return normalizedCapturedArtifactPolicyV1{}, fmt.Errorf(
			"captured artifact policy JSON exceeds %d bytes", maxCapturedArtifactPolicyBytes,
		)
	}
	if err := requireMetadataFields(canonical, []string{"roles", "version"}, nil); err != nil {
		return normalizedCapturedArtifactPolicyV1{}, fmt.Errorf("captured artifact policy: %w", err)
	}
	var wire capturedArtifactPolicyWireV1
	if err := json.Unmarshal(canonical, &wire, json.RejectUnknownMembers(true)); err != nil {
		return normalizedCapturedArtifactPolicyV1{}, fmt.Errorf("decoding captured artifact policy: %w", err)
	}
	if wire.Version != 1 {
		return normalizedCapturedArtifactPolicyV1{}, fmt.Errorf(
			"captured artifact policy version must be 1, got %d", wire.Version,
		)
	}
	if len(wire.Roles) > maxRenditionArtifacts {
		return normalizedCapturedArtifactPolicyV1{}, fmt.Errorf(
			"captured artifact policy must contain at most %d roles", maxRenditionArtifacts,
		)
	}
	policy := capturedArtifactPolicyV1{
		Roles: make([]capturedArtifactRoleV1, 0, len(wire.Roles)), Version: wire.Version,
	}
	cardinalities := make(map[string]capturedArtifactRoleV1, len(wire.Roles))
	totalMaximum := 0
	for index, rawCardinality := range wire.Roles {
		if err := requireMetadataFields(
			rawCardinality, []string{"max_count", "min_count", "role"}, nil,
		); err != nil {
			return normalizedCapturedArtifactPolicyV1{}, fmt.Errorf(
				"captured artifact policy role %d: %w", index, err,
			)
		}
		var cardinality capturedArtifactRoleV1
		if err := json.Unmarshal(rawCardinality, &cardinality, json.RejectUnknownMembers(true)); err != nil {
			return normalizedCapturedArtifactPolicyV1{}, fmt.Errorf(
				"decoding captured artifact policy role %d: %w", index, err,
			)
		}
		if !validCapturedArtifactRole(cardinality.Role) {
			return normalizedCapturedArtifactPolicyV1{}, fmt.Errorf(
				"captured artifact policy role %q is unknown", cardinality.Role,
			)
		}
		if _, duplicated := cardinalities[cardinality.Role]; duplicated {
			return normalizedCapturedArtifactPolicyV1{}, fmt.Errorf(
				"captured artifact policy role %q is duplicated", cardinality.Role,
			)
		}
		if cardinality.MinCount < 0 || cardinality.MaxCount < cardinality.MinCount ||
			cardinality.MaxCount > maxRenditionArtifacts {
			return normalizedCapturedArtifactPolicyV1{}, fmt.Errorf(
				"captured artifact policy role %q cardinality [%d,%d] is invalid",
				cardinality.Role, cardinality.MinCount, cardinality.MaxCount,
			)
		}
		totalMaximum += cardinality.MaxCount
		if totalMaximum > maxRenditionArtifacts {
			return normalizedCapturedArtifactPolicyV1{}, fmt.Errorf(
				"captured artifact policy maximum total exceeds %d", maxRenditionArtifacts,
			)
		}
		cardinalities[cardinality.Role] = cardinality
		policy.Roles = append(policy.Roles, cardinality)
	}
	sort.Slice(policy.Roles, func(i, j int) bool {
		return policy.Roles[i].Role < policy.Roles[j].Role
	})
	normalizedBytes, err := json.Marshal(policy, json.Deterministic(true))
	if err != nil {
		return normalizedCapturedArtifactPolicyV1{}, fmt.Errorf("encoding captured artifact policy: %w", err)
	}
	normalized := jsontext.Value(normalizedBytes)
	if err := normalized.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return normalizedCapturedArtifactPolicyV1{}, fmt.Errorf("canonicalizing captured artifact policy: %w", err)
	}
	return normalizedCapturedArtifactPolicyV1{
		canonical: normalized, cardinalities: cardinalities,
	}, nil
}

func validCapturedArtifactRole(role string) bool {
	switch role {
	case catalogArtifactNormalizedEvidence, catalogArtifactSanitizedMarkdown,
		string(document.EvidenceArtifactImage), string(document.EvidenceArtifactMarkdown),
		string(document.EvidenceArtifactStructured), string(document.EvidenceArtifactTranscript):
		return true
	default:
		return false
	}
}

func validateCatalogLocatorV1(locator document.EvidenceLocatorV1) error {
	if locator.Name != "" {
		if err := validateCatalogUTF8(locator.Name, maxCatalogIdentifierBytes, "locator name", false); err != nil {
			return err
		}
		if strings.TrimSpace(locator.Name) == "" || !norm.NFC.IsNormalString(locator.Name) ||
			strings.Contains(locator.Name, "\r") {
			return errors.New("locator name is not canonical")
		}
	}
	switch locator.Kind {
	case document.EvidenceLocatorGeneric, document.EvidenceLocatorMessage, document.EvidenceLocatorSection:
		if locator.IndexOrigin != document.EvidenceIndexOriginNone || locator.Start != 0 || locator.End != 0 {
			return fmt.Errorf("%s locator must not claim an index", locator.Kind)
		}
		return nil
	case document.EvidenceLocatorLine, document.EvidenceLocatorPage, document.EvidenceLocatorRecord,
		document.EvidenceLocatorSheet, document.EvidenceLocatorSlide, document.EvidenceLocatorSpine:
	default:
		return fmt.Errorf("locator kind %q is unknown", locator.Kind)
	}
	if locator.IndexOrigin != document.EvidenceIndexOriginZero &&
		locator.IndexOrigin != document.EvidenceIndexOriginOne {
		return fmt.Errorf("%s locator must declare zero- or one-based indexing", locator.Kind)
	}
	minimum := int64(0)
	if locator.IndexOrigin == document.EvidenceIndexOriginOne {
		minimum = 1
	}
	if locator.Start < minimum || locator.End < locator.Start || locator.End > maxCatalogLocatorCoordinate {
		return fmt.Errorf("%s locator has an invalid range", locator.Kind)
	}
	switch locator.Kind {
	case document.EvidenceLocatorPage, document.EvidenceLocatorSheet,
		document.EvidenceLocatorSlide, document.EvidenceLocatorSpine:
		if locator.End != locator.Start {
			return fmt.Errorf("%s locator must identify one ordered unit", locator.Kind)
		}
	case document.EvidenceLocatorGeneric, document.EvidenceLocatorLine,
		document.EvidenceLocatorMessage, document.EvidenceLocatorRecord,
		document.EvidenceLocatorSection:
		// The first switch validates these kinds completely.
	default:
		return fmt.Errorf("locator kind %q is unknown", locator.Kind)
	}
	if locator.Kind == document.EvidenceLocatorSheet && strings.TrimSpace(locator.Name) == "" {
		return errors.New("sheet locator requires a stable name")
	}
	return nil
}

func validateCatalogUTF8(value string, maxBytes int, subject string, allowEmpty bool) error {
	if (!allowEmpty && value == "") || !utf8.ValidString(value) || len(value) > maxBytes {
		return fmt.Errorf("%s must be bounded UTF-8 of at most %d bytes", subject, maxBytes)
	}
	return nil
}

func validateCatalogHeading(value string) error {
	if err := validateCatalogUTF8(value, maxCatalogHeadingBytes, "rendition heading", false); err != nil {
		return err
	}
	if !norm.NFC.IsNormalString(value) || strings.Contains(value, "\r") {
		return errors.New("rendition heading is not canonical")
	}
	return nil
}

func canonicalCatalogJSON(raw jsontext.Value, subject string) (jsontext.Value, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%s JSON is required", subject)
	}
	if len(raw) > maxCatalogJSONBytes {
		return nil, fmt.Errorf("%s JSON exceeds %d bytes", subject, maxCatalogJSONBytes)
	}
	canonical := append(jsontext.Value(nil), raw...)
	if err := canonical.Canonicalize(jsontext.CanonicalizeRawInts(false)); err != nil {
		return nil, fmt.Errorf("invalid %s JSON: %w", subject, err)
	}
	return canonical, nil
}

func mustCatalogJSON(value any) string {
	encoded, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		panic(fmt.Sprintf("encoding validated catalog JSON: %v", err))
	}
	canonical, err := canonicalCatalogJSON(encoded, "catalog")
	if err != nil {
		panic(err)
	}
	return string(canonical)
}

func digestCatalogJSON(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func validateCatalogSHA256(value, subject string) error {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return fmt.Errorf("%s must be a lowercase SHA-256 value", subject)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("%s must be a lowercase SHA-256 value", subject)
	}
	return nil
}
