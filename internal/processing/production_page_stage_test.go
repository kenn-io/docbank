package processing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/packstore"
)

func stageTestHash(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

type stageTestCatalog struct {
	stage    production.ProductionPageStage
	recorded int
	stale    bool
}

func (c *stageTestCatalog) RecordBlob(_ context.Context, _ string, _ int64, _ store.BlobPhysical) error {
	c.recorded++
	return nil
}
func (c *stageTestCatalog) StageProductionPage(_ context.Context, _ production.JobClaim, stage production.ProductionPageStage) (production.ProductionPageStage, error) {
	if c.stale {
		return production.ProductionPageStage{}, production.ErrJobStaleClaim
	}
	if c.stage.SHA256 != "" && c.stage != stage {
		return production.ProductionPageStage{}, production.ErrJobConflict
	}
	c.stage = stage
	return stage, nil
}
func (c *stageTestCatalog) LoadProductionPageStage(_ context.Context, _, _ string, _ int) (production.ProductionPageStage, error) {
	return c.stage, nil
}

type stageTestBlobs struct {
	data   []byte
	writes int
}

func (b *stageTestBlobs) WithMutation(_ context.Context, fn func() error) error { return fn() }
func (b *stageTestBlobs) WriteDetailedContext(_ context.Context, reader io.Reader) (blob.WriteReceipt, error) {
	b.writes++
	data, err := io.ReadAll(reader)
	if err != nil {
		return blob.WriteReceipt{}, err
	}
	b.data = data
	return blob.WriteReceipt{Hash: stageTestHash(string(data)), Size: int64(len(data)), StoredSize: int64(len(data)),
		Encoding: packstore.LooseEncodingRaw, Created: true, PackEligible: true}, nil
}
func (b *stageTestBlobs) OpenStreamContext(_ context.Context, _ string) (packstore.VerifiedReadCloser, int64, error) {
	return &stageTestStream{Reader: bytes.NewReader(b.data)}, int64(len(b.data)), nil
}

type stageTestStream struct{ *bytes.Reader }

func (*stageTestStream) Close() error   { return nil }
func (*stageTestStream) Verify() error  { return nil }
func (*stageTestStream) Verified() bool { return true }

func TestProductionPageStageAdapterWritesBeforeFencedMetadata(t *testing.T) {
	const jobID = "77000000-0000-4000-8000-000000000001"
	const memberID = "77000000-0000-4000-8000-000000000003"
	job := production.Job{ID: jobID, RevisionSHA256: stageTestHash("revision"), PreparedInputSHA256: stageTestHash("prepared")}
	reservation := documentproduction.NumberReservation{Contract: documentproduction.NumberReservationContractV1, Authority: "bates-ledger/v1",
		ID: "77000000-0000-4000-8000-000000000002", OperationID: jobID, RevisionSHA256: job.RevisionSHA256, State: "reserved",
		Numbers: []documentproduction.AssignedNumber{{MemberID: memberID, MemberOrdinal: 1, Page: 1, Text: "SYN000001"}}}
	_, digest, err := documentproduction.CanonicalNumberReservation(reservation)
	require.NoError(t, err)
	reservation.SHA256 = digest
	frame := redaction.Page{Number: 1, FrameSHA256: stageTestHash("frame"), Width: 85000, Height: 110000, Span: redaction.Span{Start: 0, End: 1}}
	page := production.RenderPagePlan{MemberID: memberID, MemberOrdinal: 1, Page: 1, ResolvedSHA256: stageTestHash("resolved"),
		Layout: redaction.PageLayout{Source: frame, Output: frame}, Endorsements: []redaction.Endorsement{}}
	plan, _, err := production.CanonicalRenderPlan(production.RenderPlan{Contract: production.RenderPlanContractV1, JobID: jobID,
		RevisionSHA256: job.RevisionSHA256, Reservation: reservation, Pages: []production.RenderPagePlan{page}})
	require.NoError(t, err)
	prepared := documentproduction.PreparedMember{Member: redaction.Member{ID: memberID, Ordinal: 1},
		Resolved:       redaction.Resolved{SHA256: page.ResolvedSHA256, RecipeSHA256: stageTestHash("recipe"), Pages: []redaction.Page{frame}},
		ResolvedSHA256: page.ResolvedSHA256}
	data := []byte("synthetic verified final PNG")
	file, err := os.CreateTemp(t.TempDir(), "candidate-*.png")
	require.NoError(t, err)
	_, err = file.Write(data)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	candidate := production.ProductionPageCandidate{File: file, SHA256: stageTestHash(string(data)), Size: int64(len(data))}
	catalog := &stageTestCatalog{}
	blobs := &stageTestBlobs{}
	adapter := ProductionPageStageAdapter{Catalog: catalog, Blobs: blobs}
	claim := production.JobClaim{JobID: jobID, Token: "synthetic"}
	stage, err := adapter.StageProductionPage(t.Context(), claim, job, plan, prepared, page, candidate)
	require.NoError(t, err)
	require.Equal(t, 1, blobs.writes)
	require.Equal(t, 1, catalog.recorded)
	require.Equal(t, candidate.SHA256, stage.Artifact.SHA256)
	require.Equal(t, stageTestHash(string(data)), stage.Artifact.SHA256)
	require.Equal(t, "image/png", stage.Artifact.MediaType)
	_, err = adapter.StageProductionPage(t.Context(), claim, job, plan, prepared, page, candidate)
	require.NoError(t, err)
	require.Equal(t, stage, catalog.stage)
	stream, size, err := adapter.OpenStagedProductionPage(t.Context(), stage)
	require.NoError(t, err)
	require.Equal(t, int64(len(data)), size)
	require.NoError(t, stream.Close())

	catalog.stale = true
	_, err = adapter.StageProductionPage(t.Context(), claim, job, plan, prepared, page, candidate)
	require.ErrorIs(t, err, production.ErrJobStaleClaim)
	priorWrites := blobs.writes
	candidate.SHA256 = stageTestHash("changed")
	_, err = adapter.StageProductionPage(t.Context(), claim, job, plan, prepared, page, candidate)
	require.ErrorIs(t, err, production.ErrJobConflict)
	require.Equal(t, priorWrites, blobs.writes)
}
