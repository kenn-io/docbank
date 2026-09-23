package processing

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/production"
)

type finalArtifactTestCatalog struct {
	stageTestCatalog

	artifact documentproduction.Artifact
	stale    bool
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
