package processing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/google/uuid"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

type productionPageCatalog interface {
	RecordBlob(ctx context.Context, hash string, size int64, physical store.BlobPhysical) error
	StageProductionPage(ctx context.Context, claim production.JobClaim, stage production.ProductionPageStage) (production.ProductionPageStage, error)
	LoadProductionPageStage(ctx context.Context, jobID, memberID string, page int) (production.ProductionPageStage, error)
}

// ProductionPageStageAdapter writes verified private page bytes first, then
// asks Store to publish their artifact and receipt under the live claim. A
// failed metadata transaction may leave an unreachable blob for later GC.
type ProductionPageStageAdapter struct {
	Catalog productionPageCatalog
	Blobs   emailBlobs
}

func (a ProductionPageStageAdapter) StageProductionPage(ctx context.Context, claim production.JobClaim,
	job production.Job, plan production.RenderPlan, prepared documentproduction.PreparedMember,
	page production.RenderPagePlan, candidate production.ProductionPageCandidate) (result production.ProductionPageStage, err error) {
	if a.Catalog == nil || a.Blobs == nil || candidate.File == nil || candidate.Size < 1 {
		return production.ProductionPageStage{}, production.ErrJobConflict
	}
	if err := verifyProductionCandidate(ctx, candidate.File, candidate.Size, candidate.SHA256); err != nil {
		return production.ProductionPageStage{}, err
	}
	err = a.Blobs.WithMutation(ctx, func() error {
		if _, seekErr := candidate.File.Seek(0, io.SeekStart); seekErr != nil {
			return seekErr
		}
		written, writeErr := a.Blobs.WriteDetailedContext(ctx, candidate.File)
		if writeErr != nil {
			return writeErr
		}
		if written.Hash != candidate.SHA256 || written.Size != candidate.Size {
			return production.ErrJobConflict
		}
		encoding, encodeErr := written.EncodingName()
		if encodeErr != nil {
			return encodeErr
		}
		physical := store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize,
			PackEligible: written.PackEligible, MD5: written.MD5, Created: written.Created}
		if writeErr = a.Catalog.RecordBlob(ctx, written.Hash, written.Size, physical); writeErr != nil {
			return writeErr
		}
		artifact := productionPageArtifact(job, page, written.Hash, written.Size)
		stage, stageErr := production.BuildProductionPageStage(job, plan, prepared, page, artifact)
		if stageErr != nil {
			return stageErr
		}
		result, writeErr = a.Catalog.StageProductionPage(ctx, claim, stage)
		if writeErr != nil {
			return writeErr
		}
		if result != stage || result.Artifact.SHA256 != written.Hash || result.Artifact.Size != written.Size {
			return production.ErrJobConflict
		}
		return nil
	})
	return result, err
}

func productionPageArtifact(job production.Job, page production.RenderPagePlan, hash string, size int64) documentproduction.Artifact {
	key := sha256.Sum256([]byte(fmt.Sprintf("production-page/v1\x00%s\x00%s\x00%d", job.ID, page.MemberID, page.Page)))
	var id uuid.UUID
	copy(id[:], key[:16])
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	identity := id.String()
	return documentproduction.Artifact{ID: identity, MemberID: page.MemberID, MemberOrdinal: page.MemberOrdinal,
		Page: page.Page, Role: documentproduction.ArtifactRoleRedactedPage,
		Path: "VOL001/" + identity + ".png", SHA256: hash, Size: size, MediaType: "image/png", Volume: "VOL001"}
}

func verifyProductionCandidate(ctx context.Context, file *os.File, size int64, digest string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	if stat.Size() != size {
		return production.ErrJobConflict
	}
	hasher := sha256.New()
	read, err := io.Copy(hasher, io.NewSectionReader(file, 0, size))
	if err != nil {
		return err
	}
	if read != size || hex.EncodeToString(hasher.Sum(nil)) != digest {
		return production.ErrJobConflict
	}
	return ctx.Err()
}

func (a ProductionPageStageAdapter) LoadProductionPageStage(ctx context.Context, jobID, memberID string, page int) (production.ProductionPageStage, error) {
	if a.Catalog == nil {
		return production.ProductionPageStage{}, production.ErrJobConflict
	}
	return a.Catalog.LoadProductionPageStage(ctx, jobID, memberID, page)
}

func (a ProductionPageStageAdapter) OpenStagedProductionPage(ctx context.Context, stage production.ProductionPageStage) (packstore.VerifiedReadCloser, int64, error) {
	if a.Catalog == nil || a.Blobs == nil {
		return nil, 0, production.ErrJobConflict
	}
	stored, err := a.Catalog.LoadProductionPageStage(ctx, stage.JobID, stage.MemberID, stage.Page)
	if err != nil || stored != stage {
		return nil, 0, errors.Join(production.ErrJobConflict, err)
	}
	stream, size, err := a.Blobs.OpenStreamContext(ctx, stage.Artifact.SHA256)
	if err != nil || stream == nil || size != stage.Artifact.Size {
		if stream != nil {
			err = errors.Join(err, stream.Close())
		}
		return nil, 0, errors.Join(production.ErrJobConflict, err)
	}
	return stream, size, nil
}

var _ production.ProductionPageStager = ProductionPageStageAdapter{}
var _ production.ProductionPageHandleStore = ProductionPageStageAdapter{}
