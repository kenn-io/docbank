package processing

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // Auxiliary MD5 is interoperability metadata; SHA-256 remains authoritative.
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

// StagedArtifact binds one immutable catalog member to its retained payload.
// Payload is consumed once by PublishRendition.
type StagedArtifact struct {
	ID      string
	Payload io.Reader
}

// StagedRendition contains one complete dormant catalog candidate and the
// exact version-scoped authority that may publish it.
type StagedRendition struct {
	Rendition           document.RenditionV1
	RenditionPolicy     document.RenditionPolicy
	Build               store.RenditionBuildRecord
	Attachment          store.RenditionAttachmentRecord
	Head                store.RenditionHeadRecord
	LexicalGenerationID string
	Artifacts           []StagedArtifact
}

// PublishedArtifact is the verified physical receipt for one retained member.
type PublishedArtifact struct {
	ID   string
	Hash string
	MD5  string
	Size int64
}

// PublishedRendition records the exact immutable and mutable heads selected by
// one successful publication.
type PublishedRendition struct {
	BuildID           string
	AttachmentID      string
	LexicalGeneration LexicalGeneration
	Artifacts         []PublishedArtifact
}

type renditionBlobWriter interface {
	WriteDetailedContext(ctx context.Context, reader io.Reader) (blob.WriteReceipt, error)
	WithMutation(ctx context.Context, fn func() error) error
}

type renditionPublicationCatalog interface {
	RecordRenditionBlob(
		ctx context.Context, hash string, size int64, physical store.BlobPhysical,
	) error
	StageRenditionBuild(ctx context.Context, record store.RenditionBuildRecord) error
	StageLexicalGeneration(
		ctx context.Context, generationID string,
	) (store.LexicalGeneration, error)
	PublishRenditionAndLexicalHeads(
		ctx context.Context, attachment store.RenditionAttachmentRecord,
		head store.RenditionHeadRecord, generationID string,
	) error
}

type auxiliaryChecksumCatalog interface {
	RecordVerifiedBlobChecksum(ctx context.Context, record store.BlobChecksumRecord) error
}

type verifiedBlobReader interface {
	OpenStreamContext(
		ctx context.Context, hash string,
	) (packstore.VerifiedReadCloser, int64, error)
}

// BackfillAuxiliaryChecksumTargets processes a previously selected batch. A
// bad member is reported but does not prevent later hashes from progressing.
func BackfillAuxiliaryChecksumTargets(ctx context.Context, catalog auxiliaryChecksumCatalog,
	blobs verifiedBlobReader, targets []store.BlobChecksumTarget) (int, error) {
	completed := 0
	var targetErrors error
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return completed, errors.Join(targetErrors, err)
		}
		stream, size, err := blobs.OpenStreamContext(ctx, target.BlobSHA256)
		if err != nil {
			targetErrors = errors.Join(targetErrors, fmt.Errorf("opening checksum target %s: %w", target.BlobSHA256, err))
			continue
		}
		if size != target.Size {
			closeErr := stream.Close()
			targetErrors = errors.Join(targetErrors, fmt.Errorf(
				"checksum target %s size changed: catalog=%d stream=%d",
				target.BlobSHA256, target.Size, size,
			), closeErr)
			continue
		}
		digest := md5.New() //nolint:gosec // Auxiliary MD5 never grants content authority.
		read, copyErr := io.Copy(digest, stream)
		closeErr := stream.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			targetErrors = errors.Join(targetErrors, fmt.Errorf("verifying checksum target %s: %w", target.BlobSHA256, err))
			continue
		}
		if read != target.Size {
			targetErrors = errors.Join(targetErrors, fmt.Errorf(
				"checksum target %s length changed: catalog=%d read=%d",
				target.BlobSHA256, target.Size, read,
			))
			continue
		}
		if err := catalog.RecordVerifiedBlobChecksum(ctx, store.BlobChecksumRecord{
			BlobSHA256: target.BlobSHA256, MD5: hex.EncodeToString(digest.Sum(nil)),
		}); err != nil {
			targetErrors = errors.Join(targetErrors, fmt.Errorf("recording checksum target %s: %w", target.BlobSHA256, err))
			continue
		}
		completed++
	}
	return completed, targetErrors
}

// ArtifactPublisher verifies retained bytes, stages immutable catalog and FTS
// state, then publishes only through one atomic attachment/head transaction.
type ArtifactPublisher struct {
	catalog renditionPublicationCatalog
	blobs   renditionBlobWriter
}

// NewArtifactPublisher binds publication to one vault catalog and its verified
// Docbank CAS writer.
func NewArtifactPublisher(
	catalog renditionPublicationCatalog, blobs renditionBlobWriter,
) (*ArtifactPublisher, error) {
	if catalog == nil || blobs == nil {
		return nil, errors.New("artifact publisher requires catalog and blob stores")
	}
	return &ArtifactPublisher{catalog: catalog, blobs: blobs}, nil
}

// PublishRendition writes and verifies every retained payload before the
// catalog build exists. The build and FTS generation remain unreachable until
// the final transaction inserts the attachment and flips both serving heads.
func (p *ArtifactPublisher) PublishRendition(
	ctx context.Context, staged StagedRendition,
) (PublishedRendition, error) {
	if err := validateStagedRendition(staged); err != nil {
		return PublishedRendition{}, err
	}
	var published PublishedRendition
	err := p.blobs.WithMutation(ctx, func() error {
		artifacts := make(map[string]StagedArtifact, len(staged.Artifacts))
		for _, artifact := range staged.Artifacts {
			artifacts[artifact.ID] = artifact
		}
		type verifiedArtifact struct {
			published PublishedArtifact
		}
		verified := make([]verifiedArtifact, 0, len(staged.Build.Artifacts))
		var normalizedEvidence []byte
		for _, record := range staged.Build.Artifacts {
			candidate := artifacts[record.ID]
			reader := io.LimitReader(candidate.Payload, record.Size+1)
			var evidence bytes.Buffer
			if record.Role == "normalized_evidence" {
				reader = io.TeeReader(reader, &evidence)
			}
			receipt, writeErr := p.blobs.WriteDetailedContext(ctx, reader)
			if receipt.Hash != "" {
				if err := p.recordRenditionReceipt(ctx, record.Role, receipt); err != nil {
					if writeErr != nil {
						return fmt.Errorf("publishing rendition artifact %s: %w",
							record.ID, errors.Join(writeErr, err))
					}
					return fmt.Errorf("publishing rendition artifact %s: %w", record.ID, err)
				}
			}
			if writeErr != nil {
				return fmt.Errorf("publishing rendition artifact %s: %w", record.ID, writeErr)
			}
			if receipt.Hash != record.BlobHash || receipt.Size != record.Size {
				return fmt.Errorf(
					"publishing rendition artifact %s: verified receipt %s/%d does not match catalog %s/%d",
					record.ID, receipt.Hash, receipt.Size, record.BlobHash, record.Size,
				)
			}
			publishedMD5 := ""
			if record.Role == "sanitized_markdown" {
				publishedMD5 = receipt.MD5
			}
			verified = append(verified, verifiedArtifact{
				published: PublishedArtifact{
					ID: record.ID, Hash: receipt.Hash, MD5: publishedMD5, Size: receipt.Size,
				},
			})
			if record.Role == "normalized_evidence" {
				normalizedEvidence = evidence.Bytes()
			}
		}
		if err := validateStagedRenditionGraph(staged, normalizedEvidence); err != nil {
			return err
		}
		publishedArtifacts := make([]PublishedArtifact, 0, len(verified))
		for _, artifact := range verified {
			publishedArtifacts = append(publishedArtifacts, artifact.published)
		}
		if err := p.catalog.StageRenditionBuild(ctx, staged.Build); err != nil {
			return fmt.Errorf("staging rendition catalog: %w", err)
		}
		generation, err := rebuildLexicalGeneration(ctx, p.catalog, staged.LexicalGenerationID)
		if err != nil {
			return fmt.Errorf("rebuilding lexical generation: %w", err)
		}
		if err := p.catalog.PublishRenditionAndLexicalHeads(
			ctx, staged.Attachment, staged.Head, generation.ID,
		); err != nil {
			return fmt.Errorf("publishing rendition generation: %w", err)
		}
		published = PublishedRendition{
			BuildID: staged.Build.ID, AttachmentID: staged.Attachment.ID,
			LexicalGeneration: generation, Artifacts: publishedArtifacts,
		}
		return nil
	})
	if err != nil {
		return PublishedRendition{}, err
	}
	return published, nil
}

func (p *ArtifactPublisher) recordRenditionReceipt(
	ctx context.Context, role string, receipt blob.WriteReceipt,
) error {
	encoding, err := receipt.EncodingName()
	if err != nil {
		return err
	}
	physical := store.BlobPhysical{
		Encoding: encoding, StoredBytes: receipt.StoredSize,
		PackEligible: receipt.PackEligible, Created: receipt.Created,
	}
	if role == "sanitized_markdown" {
		physical.MD5 = receipt.MD5
	}
	if err := p.catalog.RecordRenditionBlob(ctx, receipt.Hash, receipt.Size, physical); err != nil {
		return fmt.Errorf("recording retained bytes as rendition staging: %w", err)
	}
	return nil
}

func validateStagedRendition(staged StagedRendition) error {
	if staged.Build.ID == "" || staged.Attachment.ID == "" || staged.LexicalGenerationID == "" {
		return errors.New("staged rendition lacks exact publication identities")
	}
	if staged.Attachment.BuildID != staged.Build.ID || staged.Head.AttachmentID != staged.Attachment.ID ||
		staged.Head.ContentVersionID != staged.Attachment.ContentVersionID ||
		staged.Head.ProcessingProfileFingerprint != staged.Attachment.Profile.Fingerprint {
		return errors.New("staged rendition head does not resolve through its exact attachment and build")
	}
	if err := store.ValidateProcessingProfileRecord(staged.Attachment.Profile); err != nil {
		return fmt.Errorf("staged rendition profile is invalid: %w", err)
	}
	if err := store.ValidateRenditionBuildRecord(staged.Build); err != nil {
		return fmt.Errorf("staged rendition build is invalid: %w", err)
	}
	var profile document.ProcessingProfileV1
	if err := json.Unmarshal(
		staged.Attachment.Profile.CanonicalProfile, &profile, json.RejectUnknownMembers(true),
	); err != nil {
		return fmt.Errorf("staged rendition canonical profile is invalid: %w", err)
	}
	if profile.Rendition == nil {
		return errors.New("staged rendition profile has no rendition binding")
	}
	if staged.Build.RenditionRequestFingerprint != staged.Attachment.Profile.RenditionRequestFingerprint ||
		staged.Build.EvidenceLexicalFingerprint != staged.Attachment.Profile.EvidenceLexicalFingerprint {
		return errors.New("staged rendition build does not match canonical profile components")
	}
	_, expectedRenditionPolicy, err := document.RenditionExecutionPoliciesForProfileV1(profile)
	if err != nil {
		return fmt.Errorf("staged rendition canonical profile policies: %w", err)
	}
	if staged.RenditionPolicy.Identity() != expectedRenditionPolicy.Identity() {
		return fmt.Errorf(
			"staged rendition policy %+v does not match canonical profile rendition limits %+v",
			staged.RenditionPolicy.Limits(), expectedRenditionPolicy.Limits(),
		)
	}
	if len(staged.Rendition.Units) > profile.Rendition.MaxUnits {
		return fmt.Errorf("staged rendition has %d units, exceeding canonical profile max units %d",
			len(staged.Rendition.Units), profile.Rendition.MaxUnits)
	}
	var declaredResponseBytes int64
	for _, artifact := range staged.Build.Artifacts {
		if artifact.Size > profile.Rendition.MaxResponseBytes-declaredResponseBytes {
			return fmt.Errorf(
				"staged rendition declares more than canonical profile max response bytes %d",
				profile.Rendition.MaxResponseBytes,
			)
		}
		declaredResponseBytes += artifact.Size
	}
	if err := validateRetainedArtifactPolicy(profile, staged.Build.Artifacts); err != nil {
		return err
	}
	if staged.Rendition.ContractVersion != document.RenditionContractV1 ||
		staged.Rendition.Checksum != staged.Build.RenditionChecksum ||
		staged.Rendition.EvidenceChecksum != staged.Build.EvidenceChecksum ||
		staged.Rendition.MarkdownChecksum != staged.Build.MarkdownChecksum ||
		staged.Rendition.Completeness != staged.Build.Completeness {
		return errors.New("staged rendition does not match its immutable build")
	}
	markdownDigest := sha256.Sum256(staged.Rendition.Markdown)
	if hex.EncodeToString(markdownDigest[:]) != staged.Rendition.MarkdownChecksum {
		return errors.New("staged rendition Markdown checksum does not match its bytes")
	}

	units := make([]store.RenditionUnitRecord, len(staged.Rendition.Units))
	for index, unit := range staged.Rendition.Units {
		units[index] = store.RenditionUnitRecord{
			ID: unit.ID, EvidenceUnitID: unit.EvidenceUnitID, Order: unit.Order,
			Checksum: unit.Checksum, HeadingPath: append([]string(nil), unit.HeadingPath...),
			Locator: unit.Locator,
		}
	}
	segments := make([]store.RenditionLexicalSegmentRecord, len(staged.Rendition.LexicalSegments))
	for index, segment := range staged.Rendition.LexicalSegments {
		segments[index] = store.RenditionLexicalSegmentRecord{
			ID: segment.ID, UnitID: segment.UnitID, Order: segment.Order,
			CharStart: segment.CharStart, CharEnd: segment.CharEnd,
			Checksum: segment.Checksum, Text: segment.Text,
		}
	}
	warnings := make([]string, len(staged.Rendition.Warnings))
	for index, warning := range staged.Rendition.Warnings {
		warnings[index] = warning.Code
	}
	if !reflect.DeepEqual(units, staged.Build.Units) ||
		!reflect.DeepEqual(segments, staged.Build.LexicalSegments) ||
		!slices.Equal(warnings, staged.Build.Warnings) {
		return errors.New("staged rendition units, lexical segments, or warnings disagree with its build")
	}

	if len(staged.Artifacts) != len(staged.Build.Artifacts) {
		return errors.New("staged rendition retained payload membership is incomplete")
	}
	declared := make(map[string]store.RenditionArtifactRecord, len(staged.Build.Artifacts))
	for _, artifact := range staged.Build.Artifacts {
		if _, exists := declared[artifact.ID]; exists {
			return fmt.Errorf("staged rendition artifact %q is duplicated", artifact.ID)
		}
		declared[artifact.ID] = artifact
		if artifact.Role == "sanitized_markdown" && artifact.BlobHash != staged.Rendition.MarkdownChecksum {
			return errors.New("staged sanitized Markdown artifact disagrees with rendition bytes")
		}
		if artifact.Role == "normalized_evidence" && artifact.BlobHash != staged.Rendition.EvidenceChecksum {
			return errors.New("staged normalized evidence artifact disagrees with rendition bytes")
		}
	}
	seen := make(map[string]bool, len(staged.Artifacts))
	for _, artifact := range staged.Artifacts {
		if artifact.ID == "" || artifact.Payload == nil {
			return errors.New("staged rendition artifact payload is incomplete")
		}
		if _, exists := declared[artifact.ID]; !exists || seen[artifact.ID] {
			return fmt.Errorf("staged rendition artifact payload %q is not exact membership", artifact.ID)
		}
		seen[artifact.ID] = true
	}
	return nil
}

func validateRetainedArtifactPolicy(
	profile document.ProcessingProfileV1, artifacts []store.RenditionArtifactRecord,
) error {
	for _, artifact := range artifacts {
		var requestedRole document.EvidenceArtifactRole
		switch artifact.Role {
		case "normalized_evidence":
			continue
		case "sanitized_markdown":
			if !profile.RetentionDisclosure.RetainSanitizedMarkdown {
				return errors.New("staged rendition includes sanitized Markdown without sanitized Markdown retention")
			}
			continue
		case string(document.EvidenceArtifactMarkdown):
			if !profile.RetentionDisclosure.RetainProviderMarkdown {
				return errors.New("staged rendition includes provider Markdown without provider Markdown retention")
			}
			requestedRole = document.EvidenceArtifactMarkdown
		case string(document.EvidenceArtifactImage):
			requestedRole = document.EvidenceArtifactImage
		case string(document.EvidenceArtifactStructured):
			requestedRole = document.EvidenceArtifactStructured
		case string(document.EvidenceArtifactTranscript):
			requestedRole = document.EvidenceArtifactTranscript
		default:
			return fmt.Errorf("staged rendition artifact role %q is unknown", artifact.Role)
		}
		if requestedRole != document.EvidenceArtifactMarkdown &&
			!profile.RetentionDisclosure.RetainTypedArtifacts {
			return fmt.Errorf("staged rendition includes %q without typed artifact retention", artifact.Role)
		}
		if !slices.Contains(profile.Rendition.RequestedArtifacts, requestedRole) {
			return fmt.Errorf("staged rendition artifact role %q was not requested by the canonical profile",
				artifact.Role)
		}
	}
	return nil
}

func validateStagedRenditionGraph(staged StagedRendition, normalizedEvidence []byte) error {
	evidence, err := canonical.DecodeWith(normalizedEvidence,
		func(value document.NormalizedEvidenceV1) ([]byte, error) {
			encoded, _, err := document.MarshalNormalizedEvidenceV1(value)
			return encoded, err
		})
	if err != nil {
		return fmt.Errorf("staged normalized evidence is invalid: %w", err)
	}
	evidenceDigest := sha256.Sum256(normalizedEvidence)
	if hex.EncodeToString(evidenceDigest[:]) != staged.Rendition.EvidenceChecksum {
		return errors.New("staged normalized evidence checksum does not match the rendition")
	}
	expected, err := document.BuildRenditionV1(evidence, staged.RenditionPolicy)
	if err != nil {
		return fmt.Errorf("rebuilding staged rendition from normalized evidence: %w", err)
	}
	if !reflect.DeepEqual(expected, staged.Rendition) {
		return errors.New("staged rendition does not match deterministic producer output")
	}
	return nil
}
