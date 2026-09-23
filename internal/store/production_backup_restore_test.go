package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/emailmime"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

func productionBackupDriver(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "production-backup-driver")
	if os.PathSeparator == '\\' {
		path += ".exe"
	}
	command := exec.CommandContext(t.Context(), "go", "build", "-tags", "fts5", "-o", path,
		"./testdata/production_backup_driver.go")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build backup driver: %v\n%s", err, output)
	}
	return path
}

func runProductionBackupDriver(t *testing.T, driver, action, source, target string) error {
	t.Helper()
	output, err := exec.CommandContext(t.Context(), driver, action, source, target).CombinedOutput()
	if err != nil {
		return &productionBackupDriverError{err: err, output: string(output)}
	}
	return nil
}

type productionBackupDriverError struct {
	err    error
	output string
}

func (e *productionBackupDriverError) Error() string { return e.err.Error() + ": " + e.output }

func materializeProductionEmailBlobs(t *testing.T, f *realRestartFixture) {
	t.Helper()
	source := []byte(catalogEmailSource)
	hash := sha256.Sum256(source)
	decoded, err := emailmime.Decode(t.Context(), hex.EncodeToString(hash[:]), int64(len(source)),
		bytes.NewReader(source), t.TempDir())
	require.NoError(t, err)
	defer func() { require.NoError(t, decoded.Close()) }()
	write := func(data []byte) {
		t.Helper()
		_, err := f.blobs.loose.Write(t.Context(), bytes.NewReader(data), packstore.WriteOptions{
			Durability: packstore.DurablePublication, Dedup: packstore.VerifyFullHash, MaxBytes: 128 << 20,
		})
		require.NoError(t, err)
	}
	write(source)
	for _, artifact := range decoded.Artifacts() {
		stream, err := decoded.OpenArtifact(t.Context(), artifact.PartPath, string(artifact.Reference.Role))
		require.NoError(t, err)
		data, err := io.ReadAll(stream)
		require.NoError(t, err)
		require.NoError(t, stream.Close())
		write(data)
	}
}

func productionBackupMetadata(t *testing.T, s *Store) []byte {
	t.Helper()
	var data bytes.Buffer
	snapshot, err := s.BeginMetadataSnapshot(t.Context())
	require.NoError(t, err)
	require.NoError(t, snapshot.ExportBackup(t.Context(), &data))
	require.NoError(t, snapshot.Close())
	return data.Bytes()
}

func productionAuthorityJSONL(t *testing.T, data []byte) []byte {
	t.Helper()
	require.Contains(t, string(bytes.SplitN(data, []byte{'\n'}, 2)[0]), `"version":1`)
	var selected [][]byte
	for line := range bytes.SplitSeq(bytes.TrimSpace(data), []byte{'\n'}) {
		var head struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(line, &head))
		if strings.HasPrefix(head.Type, "production") || strings.HasPrefix(head.Type, "bates") ||
			strings.HasPrefix(head.Type, "rendition") || head.Type == "collection_snapshot" {
			selected = append(selected, line)
		}
	}
	require.NotEmpty(t, selected)
	return bytes.Join(selected, []byte{'\n'})
}

func requireProductionBackupMetadataEqual(t *testing.T, want, got []byte) {
	t.Helper()
	want = productionAuthorityJSONL(t, want)
	got = productionAuthorityJSONL(t, got)
	if bytes.Equal(want, got) {
		return
	}
	wantLines, gotLines := bytes.Split(want, []byte{'\n'}), bytes.Split(got, []byte{'\n'})
	for index := range min(len(wantLines), len(gotLines)) {
		if !bytes.Equal(wantLines[index], gotLines[index]) {
			t.Fatalf("backup metadata row %d differs: want %q; got %q", index,
				wantLines[index][:min(200, len(wantLines[index]))],
				gotLines[index][:min(200, len(gotLines[index]))])
		}
	}
	t.Fatalf("backup metadata row count differs: want %d; got %d", len(wantLines), len(gotLines))
}

func productionBackupBlobBytes(t *testing.T, f *realRestartFixture) map[string][]byte {
	t.Helper()
	rows, err := f.db.QueryContext(t.Context(), BackupBlobAuthorityCTE()+
		`SELECT hash FROM backup_authorized_blobs ORDER BY hash`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	result := make(map[string][]byte)
	for rows.Next() {
		var hash string
		require.NoError(t, rows.Scan(&hash))
		stream, _, err := f.blobs.OpenStreamContext(t.Context(), hash)
		require.NoError(t, err, hash)
		data, err := io.ReadAll(stream)
		require.NoError(t, err, hash)
		require.NoError(t, stream.Verify(), hash)
		require.NoError(t, stream.Close(), hash)
		result[hash] = data
	}
	require.NoError(t, rows.Err())
	return result
}

func requireProductionBackupBlobsEqual(t *testing.T, want, got map[string][]byte) {
	t.Helper()
	require.Len(t, got, len(want))
	for hash, expected := range want {
		actual, ok := got[hash]
		require.True(t, ok, "restored blob %s is missing", hash)
		require.True(t, bytes.Equal(expected, actual), "restored blob %s differs", hash)
	}
}

func requireProductionBackupRestored(t *testing.T, target string, wantMetadata []byte, wantBlobs map[string][]byte) *realRestartFixture {
	t.Helper()
	s, err := Open(filepath.Join(target, "docbank.db"))
	require.NoError(t, err)
	f := &realRestartFixture{Store: s, root: target}
	f.blobs, err = openRestartBlobs(s, target)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, f.blobs.Close())
		require.NoError(t, f.Close())
	})
	requireProductionBackupMetadataEqual(t, wantMetadata, productionBackupMetadata(t, s))
	requireProductionBackupBlobsEqual(t, wantBlobs, productionBackupBlobBytes(t, f))
	require.NoError(t, s.ValidateMetadata(t.Context()))
	require.NoError(t, s.VerifyRenditionBlobAuthority(t.Context()))
	require.NoError(t, s.VerifyRenditionBlobBytes(t.Context(), f.blobs))
	return f
}

func TestProductionBackupRestoreResumesAndRetainsFinalArtifacts(t *testing.T) {
	driver := productionBackupDriver(t)
	s, finalized, job := unreservedProductionCheckpointFixture(t)
	f := &realRestartFixture{Store: s, root: filepath.Dir(s.path)}
	var err error
	f.blobs, err = openRestartBlobs(s, f.root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.blobs.Close()) })
	materializeProductionEmailBlobs(t, f)
	pdf := []byte("synthetic derived PDF")
	written, err := f.write(t.Context(), bytes.NewReader(pdf))
	require.NoError(t, err)
	require.Equal(t, finalized.Authority.Prepared.Members[0].Member.PDFSHA256, written.Hash)
	request := production.JobRequest{JobID: job.ID, OperationID: job.OperationID, SetID: job.SetID,
		Revision: job.Revision, ETag: job.ETag, RevisionSHA256: job.RevisionSHA256,
		PreparedInputSHA256:    job.PreparedInputSHA256,
		NumberingProfileSHA256: finalized.Draft.NumberingRecipeSHA256}
	f.fault = documentproduction.ArtifactRoleRedactedPDF
	_, err = f.worker().RunJob(t.Context(), request)
	require.ErrorIs(t, err, errLostProductionResponse)
	var allocations, stages, members int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM bates_allocations WHERE operation_id=?`, job.ID).Scan(&allocations))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM production_job_page_stages WHERE job_id=?`, job.ID).Scan(&stages))
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM production_members WHERE set_id=?`, job.SetID).Scan(&members))
	require.Equal(t, 1, allocations)
	require.Equal(t, 2, stages)
	require.Equal(t, 2, members)
	_, err = s.db.ExecContext(t.Context(), `UPDATE production_jobs SET lease_expires_at=? WHERE job_id=?`,
		"2020-01-01T00:00:00Z", job.ID)
	require.NoError(t, err)
	before := productionBackupMetadata(t, s)
	physical := productionBackupBlobBytes(t, f)
	beforePlan, err := s.LoadProductionRenderPlan(t.Context(), job.ID)
	require.NoError(t, err)
	repo := filepath.Join(t.TempDir(), "interrupted-repo")
	require.NoError(t, runProductionBackupDriver(t, driver, "create", f.root, repo))
	target := filepath.Join(t.TempDir(), "interrupted-restore")
	require.NoError(t, runProductionBackupDriver(t, driver, "restore", repo, target))
	restored := requireProductionBackupRestored(t, target, before, physical)
	restoredFinalized, err := restored.LoadFinalizedProduction(t.Context(), job.SetID, job.Revision)
	require.NoError(t, err)
	require.Equal(t, finalized, restoredFinalized)
	restoredPlan, err := restored.LoadProductionRenderPlan(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, beforePlan, restoredPlan)
	require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM bates_allocations WHERE operation_id=?`, job.ID).Scan(&allocations))
	require.Equal(t, 1, allocations)
	result, err := restored.worker().RunJob(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, production.ProductionJobSucceeded, result.State)
	require.Zero(t, restored.pageWrites, "verified stages should be reused")
	var publishedArtifacts int
	require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM production_job_artifacts WHERE job_id=?`, job.ID).Scan(&publishedArtifacts))
	require.Equal(t, len(result.Manifest.Artifacts), publishedArtifacts)
	replay, err := restored.worker().RunJob(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, result.Receipt, replay.Receipt)
	require.Equal(t, result.Manifest, replay.Manifest)
	require.Equal(t, result.Reservation, replay.Reservation)
	require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM bates_allocations WHERE operation_id=?`, job.ID).Scan(&allocations))
	require.Equal(t, 1, allocations)
	var replayedArtifacts int
	require.NoError(t, restored.db.QueryRow(`SELECT COUNT(*) FROM production_job_artifacts WHERE job_id=?`, job.ID).Scan(&replayedArtifacts))
	require.Equal(t, publishedArtifacts, replayedArtifacts)

	finalMetadata := productionBackupMetadata(t, restored.Store)
	finalBlobs := productionBackupBlobBytes(t, restored)
	finalRepo := filepath.Join(t.TempDir(), "final-repo")
	require.NoError(t, runProductionBackupDriver(t, driver, "create", target, finalRepo))
	finalTarget := filepath.Join(t.TempDir(), "final-restore")
	require.NoError(t, runProductionBackupDriver(t, driver, "restore", finalRepo, finalTarget))
	reopened := requireProductionBackupRestored(t, finalTarget, finalMetadata, finalBlobs)
	retained, err := reopened.LoadProductionJob(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, result.Receipt, retained.Receipt)
	require.Equal(t, result.Manifest, retained.Manifest)
	retainedPlan, err := reopened.LoadProductionRenderPlan(t.Context(), job.ID)
	require.NoError(t, err)
	require.Equal(t, result.Reservation, retainedPlan.Reservation)
	require.Equal(t, production.ProductionJobSucceeded, retained.State)
	for _, artifact := range result.Manifest.Artifacts {
		loaded, err := reopened.LoadProductionJobArtifact(t.Context(), job.ID, artifact.ID)
		require.NoError(t, err)
		require.Equal(t, artifact, loaded)
	}

	var finalHash string
	for _, artifact := range result.Manifest.Artifacts {
		if artifact.Role == documentproduction.ArtifactRoleRedactedText {
			finalHash = artifact.SHA256
			break
		}
	}
	require.NotEmpty(t, finalHash)
	archive, err := backup.Open(finalRepo)
	require.NoError(t, err)
	index, err := archive.LoadBlobIndex()
	require.NoError(t, err)
	id, err := pack.ParseBlobID(finalHash)
	require.NoError(t, err)
	entry, ok := index[id]
	require.True(t, ok, "final text blob must be captured")
	packPath := archive.Path("packs", entry.PackID[:2], entry.PackID+packstore.PackExt)
	original, err := os.ReadFile(packPath)
	require.NoError(t, err)
	for _, damage := range []string{"missing", "tampered"} {
		t.Run(damage, func(t *testing.T) {
			if damage == "missing" {
				require.NoError(t, os.Remove(packPath))
			} else {
				changed := bytes.Clone(original)
				changed[entry.Offset+entry.StoredLen/2] ^= 1
				require.NoError(t, os.WriteFile(packPath, changed, 0o600))
			}
			t.Cleanup(func() { require.NoError(t, os.WriteFile(packPath, original, 0o600)) })
			failedTarget := filepath.Join(t.TempDir(), "unpublished")
			require.Error(t, runProductionBackupDriver(t, driver, "restore", finalRepo, failedTarget))
			_, err := os.Stat(filepath.Join(failedTarget, "docbank.db"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}
