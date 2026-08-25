package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"reflect"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
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
			physical  store.BlobPhysical
		}
		verified := make([]verifiedArtifact, 0, len(staged.Build.Artifacts))
		var normalizedEvidence []byte
		for _, record := range staged.Build.Artifacts {
			candidate := artifacts[record.ID]
			reader := candidate.Payload
			var evidence bytes.Buffer
			if record.Role == "normalized_evidence" {
				reader = io.TeeReader(reader, &evidence)
			}
			receipt, err := p.blobs.WriteDetailedContext(ctx, reader)
			if err != nil {
				return fmt.Errorf("publishing rendition artifact %s: %w", record.ID, err)
			}
			if receipt.Hash != record.BlobHash || receipt.Size != record.Size {
				return fmt.Errorf(
					"publishing rendition artifact %s: verified receipt %s/%d does not match catalog %s/%d",
					record.ID, receipt.Hash, receipt.Size, record.BlobHash, record.Size,
				)
			}
			encoding, err := receipt.EncodingName()
			if err != nil {
				return fmt.Errorf("publishing rendition artifact %s: %w", record.ID, err)
			}
			physical := store.BlobPhysical{
				Encoding: encoding, StoredBytes: receipt.StoredSize,
				PackEligible: receipt.PackEligible, Created: receipt.Created,
			}
			verified = append(verified, verifiedArtifact{
				published: PublishedArtifact{ID: record.ID, Hash: receipt.Hash, Size: receipt.Size},
				physical:  physical,
			})
			if record.Role == "normalized_evidence" {
				normalizedEvidence = append([]byte(nil), evidence.Bytes()...)
			}
		}
		if err := validateStagedRenditionGraph(staged, normalizedEvidence); err != nil {
			return err
		}
		publishedArtifacts := make([]PublishedArtifact, 0, len(verified))
		for _, artifact := range verified {
			if err := p.catalog.RecordRenditionBlob(
				ctx, artifact.published.Hash, artifact.published.Size, artifact.physical,
			); err != nil {
				return fmt.Errorf("recording rendition artifact %s: %w", artifact.published.ID, err)
			}
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

func validateStagedRendition(staged StagedRendition) error {
	if staged.Build.ID == "" || staged.Attachment.ID == "" || staged.LexicalGenerationID == "" {
		return errors.New("staged rendition lacks exact publication identities")
	}
	if staged.Attachment.BuildID != staged.Build.ID || staged.Head.AttachmentID != staged.Attachment.ID ||
		staged.Head.ContentVersionID != staged.Attachment.ContentVersionID ||
		staged.Head.ProcessingProfileFingerprint != staged.Attachment.Profile.Fingerprint {
		return errors.New("staged rendition head does not resolve through its exact attachment and build")
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
		!reflect.DeepEqual(warnings, staged.Build.Warnings) {
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

func validateStagedRenditionGraph(staged StagedRendition, normalizedEvidence []byte) error {
	var evidence document.NormalizedEvidenceV1
	if err := json.Unmarshal(normalizedEvidence, &evidence, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("staged normalized evidence is invalid: %w", err)
	}
	canonicalEvidence, evidenceChecksum, err := document.MarshalNormalizedEvidenceV1(evidence)
	if err != nil {
		return fmt.Errorf("staged normalized evidence is invalid: %w", err)
	}
	if !bytes.Equal(canonicalEvidence, normalizedEvidence) {
		return errors.New("staged normalized evidence is not exact canonical bytes")
	}
	if evidenceChecksum != staged.Rendition.EvidenceChecksum {
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
