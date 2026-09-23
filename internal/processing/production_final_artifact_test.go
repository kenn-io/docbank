package processing

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/redactiontest"
)

type finalArtifactTestCatalog struct {
	stageTestCatalog

	artifact documentproduction.Artifact
	stale    bool
}

func TestProductionFinalTextUsesOnlySealedResolvedRuns(t *testing.T) {
	const memberID = "77000000-0000-4000-8000-000000000003"
	m := redactiontest.Map("ABC")
	decision := redaction.Decision{ID: "77000000-0000-4000-8000-000000000004", MemberID: memberID,
		Action: "redact", Reason: "synthetic private reason", Selector: redaction.Selector{
			Kind: "text", MapSHA256: m.SHA256, Span: &redaction.Span{Start: 1, End: 2},
		}}
	resolved, err := redaction.Resolve(m, "redact_selected", []redaction.Decision{decision}, pdfproduction.QualifiedRecipe())
	require.NoError(t, err)
	member := documentproduction.PreparedMember{Member: redaction.Member{ID: memberID, Ordinal: 1,
		SourceVersionID: "77000000-0000-4000-8000-000000000005"},
		Resolved: resolved, ResolvedSHA256: resolved.SHA256, Decisions: []redaction.Decision{decision}}
	job := production.Job{ID: "77000000-0000-4000-8000-000000000001"}
	claim := production.JobClaim{JobID: job.ID, Token: "synthetic", Epoch: 1}
	catalog := &finalArtifactTestCatalog{}
	blobs := &stageTestBlobs{}
	adapter := ProductionFinalArtifactAdapter{Catalog: catalog, Blobs: blobs}
	artifact, err := adapter.StageVerifiedProductionText(t.Context(), claim, job, member, 1<<20)
	require.NoError(t, err)
	require.Equal(t, documentproduction.ArtifactRoleRedactedText, artifact.Role)
	require.Equal(t, "A[REDACTED]C\f", string(blobs.data))
	require.NotContains(t, string(blobs.data), decision.Reason)
	require.Equal(t, stageTestHash(string(blobs.data)), artifact.SHA256)
	_, err = adapter.StageVerifiedProductionText(t.Context(), claim, job, member, int64(len(blobs.data))-1)
	require.ErrorIs(t, err, production.ErrJobConflict)

	replayed, err := adapter.StageVerifiedProductionText(t.Context(), claim, job, member, 1<<20)
	require.NoError(t, err)
	require.Equal(t, artifact, replayed)
	second := member
	second.Member.ID = "77000000-0000-4000-8000-000000000006"
	second.Member.Ordinal = 2
	otherCatalog := &finalArtifactTestCatalog{}
	otherBlobs := &stageTestBlobs{}
	other := ProductionFinalArtifactAdapter{Catalog: otherCatalog, Blobs: otherBlobs}
	secondArtifact, err := other.StageVerifiedProductionText(t.Context(), claim, job, second, 1<<20)
	require.NoError(t, err)
	require.NotEqual(t, artifact.ID, secondArtifact.ID)
	require.Equal(t, artifact.SHA256, secondArtifact.SHA256)
	require.Equal(t, member.Member.SourceVersionID, second.Member.SourceVersionID)

	catalog.stale = true
	_, err = adapter.StageVerifiedProductionText(t.Context(), claim, job, member, 1<<20)
	require.ErrorIs(t, err, production.ErrJobStaleClaim)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = adapter.StageVerifiedProductionText(canceled, claim, job, member, 1<<20)
	require.ErrorIs(t, err, context.Canceled)
	member.Resolved.Runs[0].Text = "source text fallback"
	_, err = adapter.StageVerifiedProductionText(t.Context(), claim, job, member, 1<<20)
	require.ErrorIs(t, err, production.ErrJobConflict)
}

func (c *finalArtifactTestCatalog) StageProductionArtifact(_ context.Context, _ production.JobClaim, artifact documentproduction.Artifact) error {
	if c.stale {
		return production.ErrJobStaleClaim
	}
	if c.artifact.ID != "" && c.artifact != artifact {
		return production.ErrJobConflict
	}
	c.artifact = artifact
	return nil
}

func (c *finalArtifactTestCatalog) LoadProductionJobArtifact(_ context.Context, _ string, _ string) (documentproduction.Artifact, error) {
	return c.artifact, nil
}

func TestProductionFinalPDFStageAndReopen(t *testing.T) {
	data := []byte("%PDF-1.7\nsynthetic final PDF\n")
	file, err := os.CreateTemp(t.TempDir(), "final-*.pdf")
	require.NoError(t, err)
	_, err = file.Write(data)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	verified := &production.VerifiedProductionPDF{VerifiedProductionFile: &production.VerifiedProductionFile{File: file},
		SHA256: stageTestHash(string(data)), Size: int64(len(data))}
	job := production.Job{ID: "77000000-0000-4000-8000-000000000001"}
	member := documentproduction.PreparedMember{}
	member.Member.ID = "77000000-0000-4000-8000-000000000003"
	member.Member.Ordinal = 1
	claim := production.JobClaim{JobID: job.ID, Token: "synthetic", Epoch: 1}
	catalog := &finalArtifactTestCatalog{}
	blobs := &stageTestBlobs{}
	adapter := ProductionFinalArtifactAdapter{Catalog: catalog, Blobs: blobs}
	artifact, err := adapter.StageVerifiedProductionPDF(t.Context(), claim, job, member, verified)
	require.NoError(t, err)
	require.Equal(t, documentproduction.ArtifactRoleRedactedPDF, artifact.Role)
	require.Equal(t, verified.SHA256, artifact.SHA256)
	stream, size, err := adapter.OpenVerifiedProductionArtifact(t.Context(), job.ID, artifact)
	require.NoError(t, err)
	require.Equal(t, verified.Size, size)
	require.NoError(t, stream.Close())
	catalog.stale = true
	_, err = adapter.StageVerifiedProductionPDF(t.Context(), claim, job, member, verified)
	require.ErrorIs(t, err, production.ErrJobStaleClaim)
	verified.SHA256 = stageTestHash("changed")
	_, err = adapter.StageVerifiedProductionPDF(t.Context(), claim, job, member, verified)
	require.ErrorIs(t, err, production.ErrJobConflict)
}
