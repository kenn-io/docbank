package production

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/kit/packstore"
)

type syntheticFinalPublisher struct {
	plan      RenderPlan
	job       Job
	responses int
}

func (p *syntheticFinalPublisher) LoadProductionRenderPlan(context.Context, string) (RenderPlan, error) {
	return p.plan, nil
}
func (p *syntheticFinalPublisher) LoadProductionJob(context.Context, string) (Job, error) {
	return p.job, nil
}
func (p *syntheticFinalPublisher) PublishProductionJob(_ context.Context, _ JobClaim, job Job,
	receipt documentproduction.ProductionReceipt, manifest documentproduction.ArtifactManifest,
	endorsements []redaction.Endorsement) (Job, error) {
	p.responses++
	job.State, job.Receipt, job.Manifest, job.Endorsements = ProductionJobSucceeded, receipt, manifest, endorsements
	p.job = job
	return Job{}, errors.New("synthetic lost publish response")
}

type syntheticFinalArtifacts struct {
	pageData map[string][]byte
	pdfData  map[string][]byte
	stale    bool
	writes   int
}

func (a *syntheticFinalArtifacts) StageVerifiedProductionPDF(_ context.Context, _ JobClaim, _ Job,
	member documentproduction.PreparedMember, candidate *VerifiedProductionPDF) (documentproduction.Artifact, error) {
	if a.stale {
		return documentproduction.Artifact{}, ErrJobStaleClaim
	}
	data, err := io.ReadAll(candidate.File)
	if err != nil {
		return documentproduction.Artifact{}, err
	}
	if int64(len(data)) != candidate.Size || testHash(string(data)) != candidate.SHA256 {
		return documentproduction.Artifact{}, ErrJobConflict
	}
	a.writes++
	if a.pdfData == nil {
		a.pdfData = make(map[string][]byte)
	}
	a.pdfData[candidate.SHA256] = data
	return documentproduction.Artifact{ID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", MemberID: member.Member.ID,
		MemberOrdinal: member.Member.Ordinal, Role: documentproduction.ArtifactRoleRedactedPDF,
		Path: "VOL001/synthetic-final.pdf", SHA256: candidate.SHA256, Size: candidate.Size,
		MediaType: "application/pdf", Volume: "VOL001"}, nil
}

func (a *syntheticFinalArtifacts) OpenVerifiedProductionArtifact(_ context.Context, _ string,
	artifact documentproduction.Artifact) (packstore.VerifiedReadCloser, int64, error) {
	data := a.pdfData[artifact.SHA256]
	if artifact.Role == documentproduction.ArtifactRoleRedactedPage {
		data = a.pageData[artifact.SHA256]
	}
	if data == nil {
		return nil, 0, ErrJobConflict
	}
	return &syntheticVerifiedPDF{Reader: bytes.NewReader(data), verified: true}, int64(len(data)), nil
}

func testPublishVerifiedProductionFromPages(t *testing.T, finalized FinalizedProduction, job Job,
	plan RenderPlan, archive *syntheticPageArchive, recipe redaction.Recipe) {
	t.Helper()
	data := make(map[string][]byte)
	for _, handle := range archive.pages {
		data[handle.stage.Artifact.SHA256] = handle.data
	}
	artifacts := &syntheticFinalArtifacts{pageData: data}
	publisher := &syntheticFinalPublisher{plan: plan, job: job}
	claim := JobClaim{JobID: job.ID, Token: "synthetic claim", Epoch: 1}
	result, err := PublishVerifiedProductionJob(t.Context(), publisher, archive, artifacts, claim,
		job, finalized, plan, recipe)
	require.NoError(t, err, "lost acknowledgement must resolve through the persisted receipt")
	require.NoError(t, documentproduction.ValidateProductionReceipt(result.Receipt))
	require.Equal(t, 1, publisher.responses)
	require.Equal(t, 1, artifacts.writes)
	replayed, err := PublishVerifiedProductionJob(t.Context(), publisher, archive, artifacts, claim,
		job, finalized, plan, recipe)
	require.NoError(t, err)
	require.Equal(t, result.Receipt, replayed.Receipt)
	require.Equal(t, 1, publisher.responses, "historical replay cannot republish")
	require.Equal(t, 1, artifacts.writes, "historical replay cannot rewrite PDF")
	for _, artifact := range result.Manifest.Artifacts {
		if artifact.Role == documentproduction.ArtifactRoleRedactedPDF {
			original := artifacts.pdfData[artifact.SHA256]
			artifacts.pdfData[artifact.SHA256] = []byte("corrupt synthetic PDF")
			_, err = PublishVerifiedProductionJob(t.Context(), publisher, archive, artifacts, claim,
				job, finalized, plan, recipe)
			require.ErrorIs(t, err, ErrJobConflict)
			artifacts.pdfData[artifact.SHA256] = original
			break
		}
	}

	stale := &syntheticFinalArtifacts{pageData: data, stale: true}
	newPublisher := &syntheticFinalPublisher{plan: plan, job: job}
	_, err = PublishVerifiedProductionJob(t.Context(), newPublisher, archive, stale, claim,
		job, finalized, plan, recipe)
	require.ErrorIs(t, err, ErrJobStaleClaim)
	require.Zero(t, newPublisher.responses)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = PublishVerifiedProductionJob(canceled, newPublisher, archive, artifacts, claim,
		job, finalized, plan, recipe)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, newPublisher.responses)
}
