package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/production"
)

func publishedRealRetentionFixture(t *testing.T) (*realRestartFixture, production.Job) {
	t.Helper()
	s, finalized, job := unreservedProductionCheckpointFixture(t)
	f := &realRestartFixture{Store: s, root: filepath.Dir(s.path)}
	var err error
	f.blobs, err = openRestartBlobs(s, f.root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.blobs.Close()) })
	_, err = f.write(t.Context(), bytes.NewReader([]byte("synthetic derived PDF")))
	require.NoError(t, err)
	request := production.JobRequest{JobID: job.ID, OperationID: job.OperationID, SetID: job.SetID,
		Revision: job.Revision, ETag: job.ETag, RevisionSHA256: job.RevisionSHA256,
		PreparedInputSHA256:    job.PreparedInputSHA256,
		NumberingProfileSHA256: finalized.Draft.NumberingRecipeSHA256}
	_, err = f.worker().RunJob(t.Context(), request)
	require.NoError(t, err)
	published, err := f.LoadProductionJob(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, production.ProductionJobSucceeded, published.State)
	return f, published
}

func TestRetainProductionArtifactPinsVersionAndReconcilesLostResponse(t *testing.T) {
	f, job := publishedRealRetentionFixture(t)
	artifact := job.Manifest.Artifacts[0]
	retained, err := f.RetainProductionArtifact(t.Context(), job.ID, artifact.ID, f)
	require.NoError(t, err)
	require.Equal(t, artifact.SHA256, retained.Version.BlobHash)
	require.Equal(t, artifact.ID, retained.Artifact.ID)
	require.Equal(t, job.Manifest.SHA256, retained.Provenance.ArtifactManifestSHA256)
	require.False(t, retained.SourceHeadChanged)
	view, err := f.NodeViewByID(t.Context(), retained.Node.ID)
	require.NoError(t, err)
	require.Contains(t, view.Path, "/productions/"+job.ID+"/")
	provenance, err := f.NodeProvenance(t.Context(), retained.Node.ID, 10, 0)
	require.NoError(t, err)
	require.Equal(t, artifact.Path, provenance.Items[0].OriginalPath)
	tags, _, err := f.NodeTags(t.Context(), retained.Node.ID, 10, 0)
	require.NoError(t, err)
	require.True(t, slices.ContainsFunc(tags, func(tag Tag) bool { return tag.Name == "produced" }))

	// The first response was lost after the write. Reopen and reconcile the
	// exact file, version and provenance rather than creating a second node.
	f.reopen(t)
	replayed, err := f.RetainProductionArtifact(t.Context(), job.ID, artifact.ID, f)
	require.NoError(t, err)
	require.Equal(t, retained.Version.ID, replayed.Version.ID)
	require.Equal(t, retained.Node.ID, replayed.Node.ID)
	require.Equal(t, retained.Provenance, replayed.Provenance)
	var count int
	require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM provenance WHERE node_id=?`, retained.Node.ID).Scan(&count))
	require.Equal(t, 1, count)
}

func TestRetainProductionArtifactRejectsTamperedOrMissingBytes(t *testing.T) {
	for _, failure := range []string{"tampered", "missing"} {
		t.Run(failure, func(t *testing.T) {
			f, job := publishedRealRetentionFixture(t)
			artifact := job.Manifest.Artifacts[0]
			path := filepath.Join(f.root, "blobs", artifact.SHA256[:2], artifact.SHA256)
			if failure == "tampered" {
				require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("!"), int(artifact.Size)), 0o600))
			} else {
				require.NoError(t, os.Remove(path))
			}
			_, err := f.RetainProductionArtifact(t.Context(), job.ID, artifact.ID, f)
			require.Error(t, err)
			_, err = f.NodeByPath(t.Context(), "/productions/"+job.ID+"/"+artifact.ID)
			require.True(t, errors.Is(err, ErrNotFound))
		})
	}
}

func TestRetainProductionArtifactReportsChangedSourceHeadWithoutTaggingIt(t *testing.T) {
	f, job := publishedRealRetentionFixture(t)
	finalized, err := f.LoadFinalizedProduction(t.Context(), job.SetID, job.Revision)
	require.NoError(t, err)
	source := finalized.Authority.Prepared.Members[0].Member
	const newVersion = "76000000-0000-4000-8000-000000000094"
	_, err = f.db.Exec(`INSERT INTO content_versions(version_id,node_id,blob_hash,size,recorded_at,node_revision,introduced_operation_id,transition_kind)
		SELECT ?,node_id,blob_hash,size,?,node_revision+1,?,'content_replace' FROM content_versions WHERE version_id=?`,
		newVersion, nowRFC3339(), "76000000-0000-4000-8000-000000000095", source.SourceVersionID)
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE nodes SET current_version_id=?,revision=revision+1 WHERE id=?`, newVersion, source.NodeID)
	require.NoError(t, err)
	artifact := job.Manifest.Artifacts[0]
	retained, err := f.RetainProductionArtifact(t.Context(), job.ID, artifact.ID, f)
	require.NoError(t, err)
	require.True(t, retained.SourceHeadChanged)
	require.Equal(t, source.SourceVersionID, retained.Provenance.Entries[0].SourceVersionID)
	tags, _, err := f.NodeTags(t.Context(), source.NodeID, 10, 0)
	require.NoError(t, err)
	for _, tag := range tags {
		require.NotEqual(t, "produced", tag.Name)
	}
}

func TestRetainProductionArtifactRejectsNoncanonicalFrozenAuthority(t *testing.T) {
	f, job := publishedRealRetentionFixture(t)
	// Simulate stored-byte corruption past the immutable SQL guard.
	_, err := f.db.Exec(`DROP TRIGGER production_finalized_revisions_immutable_update`)
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE production_finalized_revisions
		SET prepared_input_json=' ' || prepared_input_json WHERE set_id=? AND revision=?`,
		job.SetID, job.Revision)
	require.NoError(t, err)
	artifact := job.Manifest.Artifacts[0]
	_, err = f.RetainProductionArtifact(t.Context(), job.ID, artifact.ID, f)
	require.ErrorIs(t, err, production.ErrArtifactProvenance)
	_, err = f.NodeByPath(t.Context(), "/productions/"+job.ID+"/"+artifact.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestRetainProductionArtifactReplayPinsOriginalVersionAfterSameBytesReplacement(t *testing.T) {
	f, job := publishedRealRetentionFixture(t)
	artifact := job.Manifest.Artifacts[0]
	retained, err := f.RetainProductionArtifact(t.Context(), job.ID, artifact.ID, f)
	require.NoError(t, err)
	const replacementVersion = "76000000-0000-4000-8000-000000000096"
	_, err = f.db.Exec(`INSERT INTO content_versions(version_id,node_id,blob_hash,size,mime_type,recorded_at,node_revision,introduced_operation_id,transition_kind)
		SELECT ?,node_id,blob_hash,size,mime_type,?,node_revision+1,?,'content_replace' FROM content_versions WHERE version_id=?`,
		replacementVersion, nowRFC3339(), "76000000-0000-4000-8000-000000000097", retained.Version.ID)
	require.NoError(t, err)
	_, err = f.db.Exec(`UPDATE nodes SET current_version_id=?,revision=revision+1 WHERE id=?`,
		replacementVersion, retained.Node.ID)
	require.NoError(t, err)
	replayed, err := f.RetainProductionArtifact(t.Context(), job.ID, artifact.ID, f)
	require.NoError(t, err)
	require.Equal(t, retained.Version.ID, replayed.Version.ID)
}

func TestRetainedProductionArtifactSurvivesBackupRestore(t *testing.T) {
	f, job := publishedRealRetentionFixture(t)
	materializeProductionEmailBlobs(t, f)
	artifact := job.Manifest.Artifacts[0]
	retained, err := f.RetainProductionArtifact(t.Context(), job.ID, artifact.ID, f)
	require.NoError(t, err)
	before := productionBackupMetadata(t, f.Store)
	physical := productionBackupBlobBytes(t, f)
	driver := productionBackupDriver(t)
	repo := filepath.Join(t.TempDir(), "retained-repo")
	require.NoError(t, runProductionBackupDriver(t, driver, "create", f.root, repo))
	target := filepath.Join(t.TempDir(), "retained-restore")
	require.NoError(t, runProductionBackupDriver(t, driver, "restore", repo, target))
	require.NoError(t, runProductionBackupDriver(t, driver, "gc", target, target))
	restored := requireProductionBackupRestored(t, target, before, physical)
	replayed, err := restored.RetainProductionArtifact(t.Context(), job.ID, artifact.ID, restored)
	require.NoError(t, err)
	require.Equal(t, retained.Node.ID, replayed.Node.ID)
	require.Equal(t, retained.Version.ID, replayed.Version.ID)
	require.Equal(t, retained.Provenance, replayed.Provenance)
}

func TestRetainProductionArtifactReconcilesResponseLossAfterEachWrite(t *testing.T) {
	for _, stage := range []string{"directory", "ingest", "tag", "assignment"} {
		t.Run(stage, func(t *testing.T) {
			f, job := publishedRealRetentionFixture(t)
			artifact := job.Manifest.Artifacts[0]
			injected := false
			_, err := f.retainProductionArtifact(t.Context(), job.ID, artifact.ID, f,
				func(written string) error {
					if written == stage {
						injected = true
						return errLostProductionResponse
					}
					return nil
				})
			require.True(t, injected)
			require.ErrorIs(t, err, errLostProductionResponse)
			retained, err := f.RetainProductionArtifact(t.Context(), job.ID, artifact.ID, f)
			require.NoError(t, err)
			f.reopen(t)
			replayed, err := f.RetainProductionArtifact(t.Context(), job.ID, artifact.ID, f)
			require.NoError(t, err)
			require.Equal(t, retained.Node.ID, replayed.Node.ID)
			require.Equal(t, retained.Version.ID, replayed.Version.ID)
			require.Equal(t, retained.Provenance, replayed.Provenance)
			var facts, bindings, tags int
			require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM provenance WHERE node_id=?`, retained.Node.ID).Scan(&facts))
			require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM provenance_version_bindings b
				JOIN provenance p ON p.identity=b.provenance_identity WHERE p.node_id=?`, retained.Node.ID).Scan(&bindings))
			require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM node_tags WHERE node_id=?`, retained.Node.ID).Scan(&tags))
			require.Equal(t, 1, facts)
			require.Equal(t, 1, bindings)
			require.Equal(t, 1, tags)
		})
	}
}
