package processing

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

type productionFinalArtifactCatalog interface {
	RecordBlob(ctx context.Context, hash string, size int64, physical store.BlobPhysical) error
	StageProductionArtifact(ctx context.Context, claim production.JobClaim, artifact documentproduction.Artifact) error
	LoadProductionJobArtifact(ctx context.Context, jobID, artifactID string) (documentproduction.Artifact, error)
}

// ProductionFinalArtifactAdapter stores a fresh, independently verified PDF
// under the fenced job before it can appear in a production receipt.
type ProductionFinalArtifactAdapter struct {
	Catalog productionFinalArtifactCatalog
	Blobs   emailBlobs
}

func (a ProductionFinalArtifactAdapter) StageVerifiedProductionPDF(ctx context.Context, claim production.JobClaim,
	job production.Job, member documentproduction.PreparedMember, candidate *production.VerifiedProductionPDF) (documentproduction.Artifact, error) {
	if a.Catalog == nil || a.Blobs == nil || candidate == nil || candidate.VerifiedProductionFile == nil ||
		candidate.File == nil || candidate.Size < 1 || claim.JobID != job.ID || member.Member.ID == "" || member.Member.Ordinal < 1 {
		return documentproduction.Artifact{}, production.ErrJobConflict
	}
	if err := verifyProductionCandidate(ctx, candidate.File, candidate.Size, candidate.SHA256); err != nil {
		return documentproduction.Artifact{}, err
	}
	var artifact documentproduction.Artifact
	err := a.Blobs.WithMutation(ctx, func() error {
		if _, err := candidate.File.Seek(0, io.SeekStart); err != nil {
			return err
		}
		written, err := a.Blobs.WriteDetailedContext(ctx, candidate.File)
		if err != nil {
			return err
		}
		if written.Hash != candidate.SHA256 || written.Size != candidate.Size {
			return production.ErrJobConflict
		}
		encoding, err := written.EncodingName()
		if err != nil {
			return err
		}
		if err := a.Catalog.RecordBlob(ctx, written.Hash, written.Size, store.BlobPhysical{
			Encoding: encoding, StoredBytes: written.StoredSize, PackEligible: written.PackEligible,
			MD5: written.MD5, Created: written.Created,
		}); err != nil {
			return err
		}
		key := sha256.Sum256([]byte(fmt.Sprintf("production-final-pdf/v1\x00%s\x00%s", job.ID, member.Member.ID)))
		var id uuid.UUID
		copy(id[:], key[:16])
		id[6] = id[6]&0x0f | 0x40
		id[8] = id[8]&0x3f | 0x80
		identity := id.String()
		artifact = documentproduction.Artifact{ID: identity, MemberID: member.Member.ID,
			MemberOrdinal: member.Member.Ordinal, Role: documentproduction.ArtifactRoleRedactedPDF,
			Path: "VOL001/" + identity + ".pdf", SHA256: written.Hash, Size: written.Size,
			MediaType: "application/pdf", Volume: "VOL001"}
		return a.Catalog.StageProductionArtifact(ctx, claim, artifact)
	})
	if err != nil {
		return documentproduction.Artifact{}, err
	}
	return artifact, nil
}

// OpenVerifiedProductionArtifact requires the persisted job artifact and
// catalog physical authority before returning a stream. Its caller must read
// through EOF and verify both stream integrity and the artifact hash/size.
func (a ProductionFinalArtifactAdapter) OpenVerifiedProductionArtifact(ctx context.Context, jobID string,
	artifact documentproduction.Artifact) (packstore.VerifiedReadCloser, int64, error) {
	if a.Catalog == nil || a.Blobs == nil || jobID == "" || artifact.ID == "" {
		return nil, 0, production.ErrJobConflict
	}
	stored, err := a.Catalog.LoadProductionJobArtifact(ctx, jobID, artifact.ID)
	if err != nil || stored != artifact {
		return nil, 0, errors.Join(production.ErrJobConflict, err)
	}
	stream, size, err := a.Blobs.OpenStreamContext(ctx, artifact.SHA256)
	if err != nil || stream == nil || size != artifact.Size {
		if stream != nil {
			err = errors.Join(err, stream.Close())
		}
		return nil, 0, errors.Join(production.ErrJobConflict, err)
	}
	return stream, size, nil
}

var _ production.ProductionFinalArtifactStore = ProductionFinalArtifactAdapter{}
