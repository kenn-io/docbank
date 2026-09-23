package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/kit/packstore"
)

func publishedProductionArtifactRootsFixture(t *testing.T) (*Store, production.Job, documentproduction.ArtifactManifest) {
	t.Helper()
	s, claim, job, receipt, manifest, endorsements := stagedProductionPublicationFixture(t)
	plan, err := s.LoadProductionRenderPlan(t.Context(), job.ID)
	require.NoError(t, err)
	_, err = s.db.ExecContext(t.Context(), `UPDATE production_jobs SET allocation_id=? WHERE job_id=?`, plan.Reservation.ID, job.ID)
	require.NoError(t, err)
	_, err = s.PublishProductionJob(t.Context(), claim, job, receipt, manifest, endorsements)
	require.NoError(t, err)
	return s, job, manifest
}

func TestFinalProductionArtifactsRootOnlyTheirBlobs(t *testing.T) {
	s, _, manifest := publishedProductionArtifactRootsFixture(t)
	orphan := productionHash("unrelated orphan bytes")
	require.NoError(t, s.RecordBlob(t.Context(), orphan, 7,
		BlobPhysical{Encoding: "raw", StoredBytes: 7, Created: true}))
	final := make([]documentproduction.Artifact, 0)
	for _, artifact := range manifest.Artifacts {
		if artifact.Role == documentproduction.ArtifactRoleRedactedPDF || artifact.Role == documentproduction.ArtifactRoleRedactedText {
			final = append(final, artifact)
		}
	}
	require.Len(t, final, 4)
	for _, artifact := range final {
		var authorized bool
		require.NoError(t, s.db.QueryRowContext(t.Context(), BackupBlobAuthorityCTE()+
			`SELECT EXISTS(SELECT 1 FROM backup_authorized_blobs WHERE hash=?)`, artifact.SHA256).Scan(&authorized))
		require.True(t, authorized, artifact.Role)
		var refs int
		require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM (`+
			blobReferenceRowsSQL(blobRootReferences)+`) WHERE blob_hash=?`, artifact.SHA256).Scan(&refs))
		require.Positive(t, refs, artifact.Role)
	}
	unreachable, err := s.UnreachableBlobs(t.Context())
	require.NoError(t, err)
	require.Contains(t, blobHashes(unreachable), orphan)
	for _, artifact := range final {
		require.NotContains(t, blobHashes(unreachable), artifact.SHA256)
	}
	require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO derivative_blob_purge_pending(blob_hash) VALUES(?)`, final[0].SHA256)
		return err
	}))
	purge, err := s.UnreachableDerivativePurgeBlobs(t.Context())
	require.NoError(t, err)
	require.NotContains(t, blobHashes(purge), final[0].SHA256)
	stats, err := func() (map[string]pruneBlobStats, error) {
		tx, txErr := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
		if txErr != nil {
			return nil, txErr
		}
		defer func() { _ = tx.Rollback() }()
		return versionPruneBlobStatsTx(tx, []string{final[0].SHA256})
	}()
	require.NoError(t, err)
	require.Positive(t, stats[final[0].SHA256].refs)

	var backup bytes.Buffer
	snapshot, err := s.BeginMetadataSnapshot(t.Context())
	require.NoError(t, err)
	require.NoError(t, snapshot.ExportBackup(t.Context(), &backup))
	require.NoError(t, snapshot.Close())
	for _, artifact := range final {
		require.Contains(t, backup.String(), `"type":"blob","hash":"`+artifact.SHA256+`"`)
	}
	require.NotContains(t, backup.String(), `"type":"blob","hash":"`+orphan+`"`)
}

func TestFinalProductionArtifactRootsRejectContradictoryAndMissingAuthority(t *testing.T) {
	for _, mode := range []string{"json", "typed_hash", "typed_size", "location"} {
		t.Run(mode, func(t *testing.T) {
			s, job, manifest := publishedProductionArtifactRootsFixture(t)
			artifact := manifest.Artifacts[slices.IndexFunc(manifest.Artifacts, func(a documentproduction.Artifact) bool {
				return a.Role == documentproduction.ArtifactRoleRedactedPDF
			})]
			var err error
			switch mode {
			case "json":
				_, err = s.db.ExecContext(t.Context(), `UPDATE production_job_artifacts SET artifact_json='{}' WHERE job_id=? AND artifact_id=?`, job.ID, artifact.ID)
			case "typed_hash":
				_, err = s.db.ExecContext(t.Context(), `UPDATE production_job_artifacts SET artifact_sha256=? WHERE job_id=? AND artifact_id=?`, productionHash("wrong root"), job.ID, artifact.ID)
			case "typed_size":
				_, err = s.db.ExecContext(t.Context(), `UPDATE production_job_artifacts SET artifact_size=artifact_size+1 WHERE job_id=? AND artifact_id=?`, job.ID, artifact.ID)
			case "location":
				_, err = s.db.ExecContext(t.Context(), `DELETE FROM blob_locations WHERE blob_hash=?`, artifact.SHA256)
			}
			require.NoError(t, err)
			if mode == "location" {
				require.Error(t, s.VerifyRenditionBlobAuthority(t.Context()))
			} else {
				require.Error(t, s.ValidateMetadata(t.Context()))
			}
		})
	}
}

type productionArtifactVerifyReader struct {
	store   *Store
	target  string
	missing bool
	tamper  bool
	opened  []string
}

func (r *productionArtifactVerifyReader) OpenStreamContext(ctx context.Context, hash string) (packstore.VerifiedReadCloser, int64, error) {
	r.opened = append(r.opened, hash)
	if hash == r.target && r.missing {
		return nil, 0, errors.New("synthetic missing final blob")
	}
	var size int64
	if err := r.store.db.QueryRowContext(ctx, `SELECT size FROM blobs WHERE hash=?`, hash).Scan(&size); err != nil {
		return nil, 0, err
	}
	if size > 1<<20 {
		return nil, 0, errors.New("synthetic blob exceeds fixture limit")
	}
	stream := &visualPreviewVerifiedReader{Reader: bytes.NewReader(make([]byte, size))}
	if hash == r.target && r.tamper {
		stream.verifyErr = errors.New("synthetic final blob hash mismatch")
	}
	return stream, size, nil
}

func TestFinalProductionArtifactBytesAreVerified(t *testing.T) {
	for _, mode := range []string{"missing", "tampered"} {
		t.Run(mode, func(t *testing.T) {
			s, _, manifest := publishedProductionArtifactRootsFixture(t)
			artifact := manifest.Artifacts[slices.IndexFunc(manifest.Artifacts, func(a documentproduction.Artifact) bool {
				return a.Role == documentproduction.ArtifactRoleRedactedText
			})]
			reader := &productionArtifactVerifyReader{store: s, target: artifact.SHA256,
				missing: mode == "missing", tamper: mode == "tampered"}
			require.Error(t, s.VerifyRestoredRenditionBlobBytes(t.Context(), reader))
			require.Contains(t, reader.opened, artifact.SHA256)
		})
	}
}

func TestFinalProductionArtifactRootsRoundTripMetadataV1(t *testing.T) {
	s, job, manifest := publishedProductionArtifactRootsFixture(t)
	var err error
	var current bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &current))
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(current.Bytes())))
	var reexport bytes.Buffer
	require.NoError(t, target.ExportMetadata(t.Context(), &reexport))
	requireProductionLifecycleBytesEqual(t, current.Bytes(), reexport.Bytes())
	for _, artifact := range manifest.Artifacts {
		var hash string
		var size int64
		require.NoError(t, target.db.QueryRowContext(t.Context(), `SELECT artifact_sha256,artifact_size FROM production_job_artifacts WHERE job_id=? AND artifact_id=?`, job.ID, artifact.ID).Scan(&hash, &size))
		require.Equal(t, artifact.SHA256, hash)
		require.Equal(t, artifact.Size, size)
	}

	// A v1 lifecycle stream produced before these columns had four artifact
	// values. Its manifest still covers those original bytes. Import derives
	// typed roots only after validating the bounded canonical artifact JSON.
	lines := bytes.Split(bytes.TrimSpace(current.Bytes()), []byte{'\n'})
	legacy := make([][]byte, len(lines))
	copy(legacy, lines)
	converted := 0
	for index, line := range legacy {
		var row metadataProductionLifecycle
		if json.Unmarshal(line, &row) != nil || row.Type != metadataProductionLifecycleType || row.Kind != "production_job_artifacts" {
			continue
		}
		require.Len(t, row.Values, 6)
		row.Values = []string{row.Values[0], row.Values[1], row.Values[4], row.Values[5]}
		valuesRaw, marshalErr := canonical.Marshal(row.Values)
		require.NoError(t, marshalErr)
		row.Checksum = digestProductionBytes(valuesRaw)
		legacy[index], err = canonical.Marshal(row)
		require.NoError(t, err)
		converted++
	}
	require.Equal(t, len(manifest.Artifacts), converted)
	digest := sha256.New()
	for _, table := range productionLifecycleTables {
		writeProductionLifecycleDigest(digest, table, "")
		for _, line := range legacy {
			var row metadataProductionLifecycle
			if json.Unmarshal(line, &row) == nil && row.Type == metadataProductionLifecycleType && row.Kind == table {
				writeProductionLifecycleDigest(digest, table, row.Checksum)
			}
		}
	}
	var authority metadataProductionLifecycleManifest
	require.NoError(t, json.Unmarshal(legacy[len(legacy)-1], &authority))
	authority.Checksum = hex.EncodeToString(digest.Sum(nil))
	legacy[len(legacy)-1], err = canonical.Marshal(authority)
	require.NoError(t, err)
	legacyInput := append(bytes.Join(legacy, []byte{'\n'}), '\n')
	oldTarget := newTestStore(t)
	require.NoError(t, oldTarget.ImportMetadata(t.Context(), bytes.NewReader(legacyInput)))
	var normalized bytes.Buffer
	require.NoError(t, oldTarget.ExportMetadata(t.Context(), &normalized))
	requireProductionLifecycleBytesEqual(t, current.Bytes(), normalized.Bytes())

	// The pre-lifecycle v1 form still imports without inventing production
	// rows or requiring a lifecycle manifest.
	plain := newTestStore(t)
	var empty bytes.Buffer
	require.NoError(t, plain.ExportMetadata(t.Context(), &empty))
	var emptyLines [][]byte
	for line := range bytes.SplitSeq(bytes.TrimSpace(empty.Bytes()), []byte{'\n'}) {
		if bytes.Contains(line, []byte(`"type":"production_lifecycle_manifest"`)) {
			continue
		}
		emptyLines = append(emptyLines, line)
	}
	var header metadataHeader
	require.NoError(t, json.Unmarshal(emptyLines[0], &header))
	header.ProductionLifecycle = false
	emptyLines[0], err = canonical.Marshal(header)
	require.NoError(t, err)
	preLifecycle := newTestStore(t)
	require.NoError(t, preLifecycle.ImportMetadata(t.Context(), bytes.NewReader(append(bytes.Join(emptyLines, []byte{'\n'}), '\n'))))
}

func requireProductionLifecycleBytesEqual(t *testing.T, expected, actual []byte) {
	t.Helper()
	lifecycleLines := func(raw []byte) [][]byte {
		var selected [][]byte
		for line := range bytes.SplitSeq(raw, []byte{'\n'}) {
			if bytes.HasPrefix(line, []byte(`{"type":"production_lifecycle"`)) ||
				bytes.HasPrefix(line, []byte(`{"type":"production_lifecycle_manifest"`)) {
				selected = append(selected, line)
			}
		}
		return selected
	}
	want, got := lifecycleLines(expected), lifecycleLines(actual)
	require.Len(t, got, len(want))
	for index := range want {
		if bytes.Equal(want[index], got[index]) {
			continue
		}
		at := 0
		for at < min(len(want[index]), len(got[index])) && want[index][at] == got[index][at] {
			at++
		}
		t.Fatalf("metadata row %d differs at byte %d: want=%q got=%q", index, at,
			want[index][max(0, at-30):min(len(want[index]), at+90)],
			got[index][max(0, at-30):min(len(got[index]), at+90)])
	}
}
