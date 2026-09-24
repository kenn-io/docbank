package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/production"
)

// RetainedProductionArtifact is the observed result of an exact retained
// artifact write. SourceHeadChanged reports a later source head; it never
// changes the source version recorded in Provenance.
type RetainedProductionArtifact struct {
	Node              Node
	Version           ContentVersion
	Artifact          documentproduction.Artifact
	Provenance        documentproduction.ArtifactProvenanceReceipt
	SourceHeadChanged bool
}

type productionArtifactSource struct {
	production.RetainSource
	NodeID       int64
	SourceSHA256 string
}

// RetainProductionArtifact verifies an already published job artifact, then
// ingests its existing durable blob under a stable private identity. The
// ordinary ingest, provenance and tag mutation records are the write journal:
// after a lost response, the same call reads them back before continuing.
func (s *Store) RetainProductionArtifact(ctx context.Context, jobID, artifactID string,
	opener production.PackageArtifactOpener) (RetainedProductionArtifact, error) {
	return s.retainProductionArtifact(ctx, jobID, artifactID, opener, nil)
}

// afterWrite injects a lost response at a durable write boundary in tests.
func (s *Store) retainProductionArtifact(ctx context.Context, jobID, artifactID string,
	opener production.PackageArtifactOpener, afterWrite func(string) error) (RetainedProductionArtifact, error) {
	bad := func(err error) (RetainedProductionArtifact, error) {
		return RetainedProductionArtifact{}, errors.Join(production.ErrArtifactProvenance, err)
	}
	written := func(stage string) error {
		if afterWrite != nil {
			return afterWrite(stage)
		}
		return nil
	}
	if opener == nil || validateUUIDv4(jobID) != nil || validateUUIDv4(artifactID) != nil {
		return bad(nil)
	}
	job, err := s.LoadProductionJob(ctx, jobID)
	if err != nil || job.State != production.ProductionJobSucceeded {
		return bad(err)
	}
	var authorityRaw []byte
	var preparedSHA, preparedInputSHA string
	const maxFrozenAuthorityBytes = 128 << 20
	var authoritySize int64
	err = s.db.QueryRowContext(ctx, `SELECT length(prepared_input_json) FROM production_finalized_revisions
		WHERE set_id=? AND revision=?`, job.SetID, job.Revision).Scan(&authoritySize)
	if err != nil || authoritySize < 1 || authoritySize > maxFrozenAuthorityBytes {
		return bad(err)
	}
	err = s.db.QueryRowContext(ctx, `SELECT prepared_input_json,prepared_sha256,prepared_input_sha256
		FROM production_finalized_revisions WHERE set_id=? AND revision=?`, job.SetID, job.Revision).
		Scan(&authorityRaw, &preparedSHA, &preparedInputSHA)
	if err != nil || len(authorityRaw) != int(authoritySize) ||
		preparedSHA != job.RevisionSHA256 || preparedInputSHA != job.PreparedInputSHA256 {
		return bad(err)
	}
	authority, err := canonical.Decode[documentproduction.PreparedInputAuthority](authorityRaw)
	if err != nil || documentproduction.ValidatePreparedInputAuthority(authority) != nil ||
		authority.Prepared.SHA256 != preparedSHA || authority.Receipt.SHA256 != preparedInputSHA ||
		authority.Prepared.SetID != job.SetID || authority.Prepared.Revision != job.Revision ||
		authority.Prepared.Policy.SHA256 != job.Receipt.PolicySHA256 {
		return bad(err)
	}
	sources := make([]production.RetainSource, 0, len(authority.Prepared.Members))
	byMember := make(map[string]productionArtifactSource, len(authority.Prepared.Members))
	for _, prepared := range authority.Prepared.Members {
		member := prepared.Member
		source := productionArtifactSource{
			RetainSource: production.RetainSource{MemberID: member.ID,
				MemberOrdinal: member.Ordinal, SourceVersionID: member.SourceVersionID},
			NodeID: member.NodeID, SourceSHA256: member.SourceSHA256,
		}
		version, readErr := s.ContentVersionByID(ctx, source.SourceVersionID)
		if readErr != nil || version.NodeID != source.NodeID || version.BlobHash != source.SourceSHA256 {
			return bad(readErr)
		}
		sources = append(sources, source.RetainSource)
		byMember[source.MemberID] = source
	}
	provenance, err := production.BuildArtifactProvenance(job, sources)
	if err != nil {
		return bad(err)
	}
	var artifact documentproduction.Artifact
	for _, candidate := range job.Manifest.Artifacts {
		if candidate.ID == artifactID {
			artifact = candidate
			break
		}
	}
	if artifact.ID == "" {
		return bad(ErrNotFound)
	}
	stored, err := s.LoadProductionJobArtifact(ctx, jobID, artifactID)
	if err != nil || stored != artifact {
		return bad(err)
	}
	if err := verifyRetainedProductionBytes(ctx, opener, jobID, artifact); err != nil {
		return bad(err)
	}
	source := byMember[artifact.MemberID]
	description, err := canonical.Marshal(struct {
		Contract      string `json:"contract"`
		JobID         string `json:"job_id"`
		ReceiptSHA256 string `json:"production_receipt_sha256"`
		ManifestSHA   string `json:"artifact_manifest_sha256"`
		ProvenanceSHA string `json:"provenance_sha256"`
		ArtifactID    string `json:"artifact_id"`
		SourceVersion string `json:"source_version_id"`
		MemberID      string `json:"member_id"`
		Ordinal       int64  `json:"member_ordinal"`
		Page          int    `json:"page"`
		Volume        string `json:"volume"`
	}{"production-retained-artifact/v1", job.ID, job.Receipt.SHA256, job.Manifest.SHA256,
		provenance.SHA256, artifact.ID, source.SourceVersionID, source.MemberID,
		source.MemberOrdinal, artifact.Page, artifact.Volume})
	if err != nil {
		return bad(err)
	}
	parent, err := s.MkdirAll(ctx, "/productions/"+job.ID)
	if err != nil {
		return bad(err)
	}
	if err := written("directory"); err != nil {
		return bad(err)
	}
	name := artifact.ID
	path := "/productions/" + job.ID + "/" + name
	node, err := s.NodeByPath(ctx, path)
	if errors.Is(err, ErrNotFound) {
		run, runErr := s.BeginCallerSuppliedIngest(ctx, "production-artifact", string(description))
		if runErr != nil {
			return bad(runErr)
		}
		receipt, ingestErr := s.IngestFileExactWithReceipt(ctx, run, parent.ID, name,
			artifact.SHA256, artifact.Size, artifact.MediaType, artifact.Path, "")
		if ingestErr == nil {
			node = receipt.Node
			if err := written("ingest"); err != nil {
				return bad(err)
			}
		} else if errors.Is(ingestErr, ErrExists) {
			node, err = s.NodeByPath(ctx, path)
			if err != nil {
				return bad(errors.Join(ingestErr, err))
			}
		} else {
			return bad(ingestErr)
		}
	} else if err != nil {
		return bad(err)
	}
	if node.IsDir() || node.TrashedAt != nil || node.BlobHash != artifact.SHA256 ||
		node.Size != artifact.Size || node.MimeType != artifact.MediaType {
		return bad(nil)
	}
	var matches int
	var retainedVersionID string
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MIN(b.content_version_id),'')
		FROM provenance p JOIN ingests i ON i.id=p.ingest_id
		JOIN provenance_version_bindings b ON b.provenance_identity=p.identity
		WHERE p.node_id=? AND i.source_kind='embedded:production-artifact' AND i.source_desc=?
		AND p.original_path=? AND b.basis_ref=?`, node.ID, string(description), artifact.Path,
		provenanceVersionBindingBasis).Scan(&matches, &retainedVersionID); err != nil || matches != 1 {
		return bad(err)
	}
	version, err := s.ContentVersionByID(ctx, retainedVersionID)
	if err != nil || version.NodeID != node.ID || version.BlobHash != artifact.SHA256 ||
		version.Size != artifact.Size || version.MimeType != artifact.MediaType {
		return bad(err)
	}
	tag, err := s.TagByName(ctx, "produced")
	if errors.Is(err, ErrNotFound) {
		tag, err = s.CreateTag(ctx, "produced")
		if err == nil {
			if writeErr := written("tag"); writeErr != nil {
				return bad(writeErr)
			}
		}
		if errors.Is(err, ErrExists) {
			tag, err = s.TagByName(ctx, "produced")
		}
	}
	if err != nil {
		return bad(err)
	}
	if _, err := s.AssignTag(ctx, tag.ID, node.ID, node.Revision); err != nil {
		// A lost assignment response or a concurrent retry is reconciled from
		// the durable tag row. An unrelated write error remains a failure.
		var present bool
		readErr := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM node_tags WHERE node_id=? AND tag_id=?)`,
			node.ID, tag.ID).Scan(&present)
		if readErr != nil || !present {
			return bad(errors.Join(err, readErr))
		}
	} else if err := written("assignment"); err != nil {
		return bad(err)
	}
	node, err = s.NodeByID(ctx, node.ID)
	if err != nil {
		return bad(err)
	}
	if node.IsDir() || node.TrashedAt != nil || node.BlobHash != artifact.SHA256 ||
		node.Size != artifact.Size || node.MimeType != artifact.MediaType {
		return bad(nil)
	}
	current, err := s.NodeByID(ctx, source.NodeID)
	changed := err != nil || current.CurrentVersionID != source.SourceVersionID
	return RetainedProductionArtifact{Node: node, Version: version, Artifact: artifact,
		Provenance: provenance, SourceHeadChanged: changed}, nil
}

func verifyRetainedProductionBytes(ctx context.Context, opener production.PackageArtifactOpener,
	jobID string, artifact documentproduction.Artifact) error {
	stream, size, err := opener.OpenVerifiedProductionArtifact(ctx, jobID, artifact)
	if err != nil || stream == nil || size != artifact.Size {
		if stream != nil {
			_ = stream.Close()
		}
		return fmt.Errorf("opening produced artifact: %w", errors.Join(production.ErrArtifactProvenance, err))
	}
	digest := sha256.New()
	n, copyErr := io.Copy(digest, io.LimitReader(stream, artifact.Size+1))
	verifyErr := stream.Verify()
	verified := stream.Verified()
	closeErr := stream.Close()
	if copyErr != nil || verifyErr != nil || closeErr != nil || !verified || n != artifact.Size ||
		hex.EncodeToString(digest.Sum(nil)) != artifact.SHA256 {
		return errors.Join(production.ErrArtifactProvenance, copyErr, verifyErr, closeErr)
	}
	return nil
}
