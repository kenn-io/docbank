package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/kit/packstore"
)

var errLostProductionResponse = errors.New("synthetic response loss")

// These test adapters use Kit's physical blob reader/writer and Docbank's
// actual Store. Faults return an error only after a metadata write commits.
type realRestartFixture struct {
	*Store

	blobs      *restartBlobs
	root       string
	fault      string
	pageWrites int
}

type restartBlobs struct {
	loose  *packstore.LooseStore
	reader *packstore.Store
}

func openRestartBlobs(s *Store, root string) (*restartBlobs, error) {
	layout, err := packstore.NewLayout(filepath.Join(root, "blobs"), packstore.LayoutOptions{
		Staging: packstore.StagingStoreDirectory, StagingDir: "tmp",
	})
	if err != nil {
		return nil, fmt.Errorf("layout for restart blobs: %w", err)
	}
	loose, err := packstore.NewLooseStore(layout)
	if err != nil {
		return nil, fmt.Errorf("open loose restart blobs: %w", err)
	}
	reader, err := packstore.NewStore(NewPackCatalog(s), layout, packstore.StoreOptions{})
	if err != nil {
		return nil, fmt.Errorf("open restart blob reader: %w", err)
	}
	return &restartBlobs{loose: loose, reader: reader}, nil
}

func (b *restartBlobs) Close() error {
	if err := b.reader.Close(); err != nil {
		return fmt.Errorf("close restart blob reader: %w", err)
	}
	return nil
}
func (b *restartBlobs) OpenStreamContext(ctx context.Context, digest string) (packstore.VerifiedReadCloser, int64, error) {
	stream, size, err := b.reader.OpenStream(ctx, packstore.Hash(digest))
	if err != nil {
		return nil, 0, fmt.Errorf("open restart blob: %w", err)
	}
	return stream, size, nil
}

type restartWriteReceipt struct {
	Hash string
	Size int64
}

func (f *realRestartFixture) reopen(t *testing.T) {
	t.Helper()
	require.NoError(t, f.blobs.Close())
	require.NoError(t, f.Close())
	var err error
	f.Store, err = Open(filepath.Join(f.root, "docbank.db"))
	require.NoError(t, err)
	f.blobs, err = openRestartBlobs(f.Store, f.root)
	require.NoError(t, err)
}

func (f *realRestartFixture) write(ctx context.Context, reader io.Reader) (restartWriteReceipt, error) {
	result, err := f.blobs.loose.Write(ctx, reader, packstore.WriteOptions{
		Durability: packstore.DurablePublication, Dedup: packstore.VerifyTypeAndSize,
		MaxBytes: 100 << 20,
	})
	if err != nil {
		return restartWriteReceipt{}, fmt.Errorf("write restart blob: %w", err)
	}
	if err := f.RecordBlob(ctx, result.Hash.String(), result.Size, BlobPhysical{
		Encoding: "raw", StoredBytes: result.StoredSize, Created: result.Created,
	}); err != nil {
		return restartWriteReceipt{}, err
	}
	return restartWriteReceipt{Hash: result.Hash.String(), Size: result.Size}, nil
}

func (f *realRestartFixture) ReserveProductionJobNumbers(ctx context.Context, job production.Job,
	finalized production.FinalizedProduction) (documentproduction.NumberReservation, error) {
	result, err := f.Store.ReserveProductionJobNumbers(ctx, job, finalized)
	if err == nil && f.fault == "reservation" {
		return documentproduction.NumberReservation{}, errLostProductionResponse
	}
	return result, err
}

func (f *realRestartFixture) CheckpointProductionRenderPlan(ctx context.Context, job production.Job,
	plan production.RenderPlan) (production.RenderPlan, error) {
	result, err := f.Store.CheckpointProductionRenderPlan(ctx, job, plan)
	if err == nil && f.fault == "plan" {
		return production.RenderPlan{}, errLostProductionResponse
	}
	return result, err
}

func (f *realRestartFixture) PublishProductionJob(ctx context.Context, claim production.JobClaim,
	job production.Job, receipt documentproduction.ProductionReceipt, manifest documentproduction.ArtifactManifest,
	endorsements []redaction.Endorsement) (production.Job, error) {
	result, err := f.Store.PublishProductionJob(ctx, claim, job, receipt, manifest, endorsements)
	if err == nil && f.fault == "publication" {
		return production.Job{}, errLostProductionResponse
	}
	return result, err
}

func (f *realRestartFixture) OpenPinnedProductionPDF(ctx context.Context, job production.Job,
	member documentproduction.PreparedMember) (production.PinnedProductionPDF, error) {
	handle, err := f.OpenFinalizedProductionPDF(ctx, job.SetID, job.Revision, member.Member.ID, f.blobs)
	if err != nil {
		return production.PinnedProductionPDF{}, err
	}
	return production.PinnedProductionPDF{PDFSHA256: handle.PDFSHA256, Size: handle.Size, Stream: handle.Stream}, nil
}

func restartArtifactID(key string) string {
	sum := sha256.Sum256([]byte(key))
	b := sum[:16]
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" +
		hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" + hex.EncodeToString(b[10:16])
}

func (f *realRestartFixture) StageProductionPage(ctx context.Context, claim production.JobClaim,
	job production.Job, plan production.RenderPlan, member documentproduction.PreparedMember,
	page production.RenderPagePlan, candidate production.ProductionPageCandidate) (production.ProductionPageStage, error) {
	if _, err := candidate.File.Seek(0, io.SeekStart); err != nil {
		return production.ProductionPageStage{}, err
	}
	write, err := f.write(ctx, candidate.File)
	if err != nil {
		return production.ProductionPageStage{}, err
	}
	if write.Hash != candidate.SHA256 || write.Size != candidate.Size {
		return production.ProductionPageStage{}, production.ErrJobConflict
	}
	id := restartArtifactID("page\x00" + job.ID + "\x00" + page.MemberID)
	artifact := documentproduction.Artifact{ID: id, MemberID: page.MemberID, MemberOrdinal: page.MemberOrdinal,
		Page: page.Page, Role: documentproduction.ArtifactRoleRedactedPage, Path: "VOL001/" + id + ".png",
		SHA256: write.Hash, Size: write.Size, MediaType: "image/png", Volume: "VOL001"}
	stage, err := production.BuildProductionPageStage(job, plan, member, page, artifact)
	if err != nil {
		return production.ProductionPageStage{}, err
	}
	stage, err = f.Store.StageProductionPage(ctx, claim, stage)
	if err != nil {
		return production.ProductionPageStage{}, err
	}
	f.pageWrites++
	if f.fault == "page" {
		return production.ProductionPageStage{}, errLostProductionResponse
	}
	return stage, nil
}

func (f *realRestartFixture) LoadProductionPageStage(ctx context.Context, jobID, memberID string, page int) (production.ProductionPageStage, error) {
	return f.Store.LoadProductionPageStage(ctx, jobID, memberID, page)
}

func (f *realRestartFixture) OpenStagedProductionPage(ctx context.Context, stage production.ProductionPageStage) (packstore.VerifiedReadCloser, int64, error) {
	stored, err := f.Store.LoadProductionPageStage(ctx, stage.JobID, stage.MemberID, stage.Page)
	if err != nil || stored != stage {
		return nil, 0, production.ErrJobConflict
	}
	return f.blobs.OpenStreamContext(ctx, stage.Artifact.SHA256)
}

func (f *realRestartFixture) stageFinal(ctx context.Context, claim production.JobClaim, job production.Job,
	member documentproduction.PreparedMember, role, extension, mediaType string, reader io.Reader) (documentproduction.Artifact, error) {
	write, err := f.write(ctx, reader)
	if err != nil {
		return documentproduction.Artifact{}, err
	}
	id := restartArtifactID(role + "\x00" + job.ID + "\x00" + member.Member.ID)
	artifact := documentproduction.Artifact{ID: id, MemberID: member.Member.ID, MemberOrdinal: member.Member.Ordinal,
		Role: role, Path: "VOL001/" + id + extension, SHA256: write.Hash, Size: write.Size,
		MediaType: mediaType, Volume: "VOL001"}
	if err := f.StageProductionArtifact(ctx, claim, artifact); err != nil {
		return documentproduction.Artifact{}, err
	}
	if f.fault == role {
		return documentproduction.Artifact{}, errLostProductionResponse
	}
	return artifact, nil
}

func (f *realRestartFixture) StageVerifiedProductionPDF(ctx context.Context, claim production.JobClaim,
	job production.Job, member documentproduction.PreparedMember, candidate *production.VerifiedProductionPDF) (documentproduction.Artifact, error) {
	if _, err := candidate.File.Seek(0, io.SeekStart); err != nil {
		return documentproduction.Artifact{}, err
	}
	artifact, err := f.stageFinal(ctx, claim, job, member, documentproduction.ArtifactRoleRedactedPDF,
		".pdf", "application/pdf", candidate.File)
	if err != nil {
		return artifact, err
	}
	if artifact.SHA256 != candidate.SHA256 || artifact.Size != candidate.Size {
		return documentproduction.Artifact{}, production.ErrJobConflict
	}
	return artifact, nil
}

func (f *realRestartFixture) StageVerifiedProductionText(ctx context.Context, claim production.JobClaim,
	job production.Job, member documentproduction.PreparedMember, maxBytes int64) (documentproduction.Artifact, error) {
	text, err := redaction.Text(member.Resolved)
	if err != nil || len(text) == 0 || int64(len(text)) > maxBytes {
		return documentproduction.Artifact{}, production.ErrJobConflict
	}
	return f.stageFinal(ctx, claim, job, member, documentproduction.ArtifactRoleRedactedText,
		".txt", "text/plain; charset=utf-8", bytes.NewReader(text))
}

func (f *realRestartFixture) OpenVerifiedProductionArtifact(ctx context.Context, jobID string,
	artifact documentproduction.Artifact) (packstore.VerifiedReadCloser, int64, error) {
	stored, err := f.LoadProductionJobArtifact(ctx, jobID, artifact.ID)
	if err != nil || stored != artifact {
		return nil, 0, production.ErrJobConflict
	}
	return f.blobs.OpenStreamContext(ctx, artifact.SHA256)
}

type restartWhiteEngine struct{}

func (restartWhiteEngine) Render(ctx context.Context, _ pdfproduction.Source, page redaction.Page,
	recipe redaction.Recipe) (pdfproduction.Raster, error) {
	if err := ctx.Err(); err != nil {
		return pdfproduction.Raster{}, err
	}
	return pdfproduction.Raster{Pixels: image.NewNRGBA(image.Rect(0, 0,
		int((page.Width*int64(recipe.DPI)+9999)/10000), int((page.Height*int64(recipe.DPI)+9999)/10000))),
		Page: page, DPI: recipe.DPI}, nil
}
func (restartWhiteEngine) Close() error { return nil }

func (f *realRestartFixture) worker() *production.Worker {
	return &production.Worker{Store: f, Source: f, Pages: f, Artifacts: f,
		WorkerID: "restart-worker", EngineFactory: func(redaction.Recipe) (pdfproduction.Engine, error) {
			return restartWhiteEngine{}, nil
		}}
}

func TestProductionRealStoreBlobRestart(t *testing.T) {
	s, finalized, job := unreservedProductionCheckpointFixture(t)
	f := &realRestartFixture{Store: s, root: filepath.Dir(s.path)}
	var err error
	f.blobs, err = openRestartBlobs(s, f.root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.blobs.Close()) })
	source := []byte("synthetic derived PDF")
	written, err := f.write(t.Context(), bytes.NewReader(source))
	require.NoError(t, err)
	require.Equal(t, finalized.Authority.Prepared.Members[0].Member.PDFSHA256, written.Hash)
	request := production.JobRequest{JobID: job.ID, OperationID: job.OperationID, SetID: job.SetID,
		Revision: job.Revision, ETag: job.ETag, RevisionSHA256: job.RevisionSHA256,
		PreparedInputSHA256:    job.PreparedInputSHA256,
		NumberingProfileSHA256: finalized.Draft.NumberingRecipeSHA256}

	for _, point := range []string{"reservation", "plan", "page", documentproduction.ArtifactRoleRedactedPDF,
		documentproduction.ArtifactRoleRedactedText} {
		f.fault = point
		_, err := f.worker().RunJob(t.Context(), request)
		require.ErrorIs(t, err, errLostProductionResponse, point)
		stored, err := f.LoadProductionJob(t.Context(), job.ID)
		require.NoError(t, err)
		require.NotEqual(t, production.ProductionJobSucceeded, stored.State, point)
		var allocations int
		require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM bates_allocations WHERE operation_id=?`, job.ID).Scan(&allocations))
		require.Equal(t, 1, allocations, point)
		if point == "page" {
			var epoch int64
			var token, owner string
			require.NoError(t, f.db.QueryRow(`SELECT claim_epoch,claim_token,claim_owner FROM production_jobs WHERE job_id=?`, job.ID).
				Scan(&epoch, &token, &owner))
			old := production.JobClaim{JobID: job.ID, Epoch: epoch, Token: token, Worker: owner}
			_, err = f.db.Exec(`UPDATE production_jobs SET lease_expires_at=? WHERE job_id=?`, "2020-01-01T00:00:00Z", job.ID)
			require.NoError(t, err)
			stage, err := f.Store.LoadProductionPageStage(t.Context(), job.ID,
				finalized.Authority.Prepared.Members[0].Member.ID, 1)
			require.NoError(t, err)
			require.ErrorIs(t, f.StageProductionArtifact(t.Context(), old, stage.Artifact), production.ErrJobStaleClaim)
			path := filepath.Join(f.root, "blobs", stage.Artifact.SHA256[:2], stage.Artifact.SHA256)
			original, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("!"), len(original)), 0o600))
			f.reopen(t)
			f.fault = ""
			_, err = f.worker().RunJob(t.Context(), request)
			require.Error(t, err, "changed page bytes cannot be silently replaced")
			stored, err = f.LoadProductionJob(t.Context(), job.ID)
			require.NoError(t, err)
			require.NotEqual(t, production.ProductionJobSucceeded, stored.State)
			require.NoError(t, os.WriteFile(path, original, 0o600))
			f.reopen(t)
			f.fault = point
		}
		_, err = f.db.Exec(`UPDATE production_jobs SET lease_expires_at=? WHERE job_id=?`, "2020-01-01T00:00:00Z", job.ID)
		require.NoError(t, err)
		f.reopen(t)
	}
	f.fault = "publication"
	result, err := f.worker().RunJob(t.Context(), request)
	require.NoError(t, err, "committed publication must survive a lost response")
	require.Equal(t, production.ProductionJobSucceeded, result.State)
	require.Equal(t, 2, f.pageWrites, "recovery must reuse both verified staged pages")
	f.reopen(t)
	replayed, err := f.worker().RunJob(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, result.Receipt, replayed.Receipt)
	require.Equal(t, result.Manifest, replayed.Manifest)
	require.Equal(t, result.Reservation, replayed.Reservation)
	for _, role := range []string{documentproduction.ArtifactRoleRedactedPage,
		documentproduction.ArtifactRoleRedactedPDF, documentproduction.ArtifactRoleRedactedText} {
		var artifact documentproduction.Artifact
		for _, candidate := range result.Manifest.Artifacts {
			if candidate.Role == role {
				artifact = candidate
				break
			}
		}
		require.NotEmpty(t, artifact.ID)
		path := filepath.Join(f.root, "blobs", artifact.SHA256[:2], artifact.SHA256)
		original, err := os.ReadFile(path)
		require.NoError(t, err)
		for _, damage := range []string{"altered", "missing"} {
			if damage == "altered" {
				require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("!"), len(original)), 0o600))
			} else {
				require.NoError(t, os.Remove(path))
			}
			f.reopen(t)
			_, err = f.worker().RunJob(t.Context(), request)
			require.Error(t, err, role+" "+damage)
			require.NoError(t, os.WriteFile(path, original, 0o600))
			f.reopen(t)
			replayed, err = f.worker().RunJob(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, result.Receipt, replayed.Receipt)
		}
	}
	_, err = f.Store.PublishProductionJob(t.Context(), production.JobClaim{JobID: job.ID, Epoch: -1,
		Worker: "stale-worker", Token: "stale"}, result, result.Receipt, result.Manifest, result.Endorsements)
	require.ErrorIs(t, err, production.ErrJobStaleClaim)
	changed := request
	changed.OperationID = "76000000-0000-4000-8000-000000000099"
	_, err = f.worker().RunJob(t.Context(), changed)
	require.ErrorIs(t, err, ErrPackageConflict)
	var allocations int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM bates_allocations WHERE operation_id=?`, job.ID).Scan(&allocations))
	require.Equal(t, 1, allocations)
}
