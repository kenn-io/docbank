package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/production"
)

func TestProductionLifecycleMetadataRoundTrip(t *testing.T) {
	s, claim, stage := productionPageStageFixture(t)
	require.NoError(t, s.RequeuePageJobs(t.Context()))
	pageClaim, err := s.ClaimPageJob(t.Context())
	require.NoError(t, err)
	require.NoError(t, s.FinishPageJob(t.Context(), pageClaim, PageJobFailed, "interrupted"))
	require.NoError(t, validateProductionMetadataState(t.Context(), s.db))
	require.NoError(t, validateBatesMetadataState(t.Context(), s.db))
	require.NoError(t, validateBatesArtifactState(t.Context(), s.db))
	require.NoError(t, validatePackageMetadataState(t.Context(), s.db, s.vaultID))
	require.NoError(t, validatePackageImportMetadataState(t.Context(), s.db))
	_, err = s.StageProductionPage(t.Context(), claim, stage)
	require.NoError(t, err)
	job, err := s.LoadProductionJob(t.Context(), claim.JobID)
	require.NoError(t, err)
	plan, err := s.LoadProductionRenderPlan(t.Context(), claim.JobID)
	require.NoError(t, err)
	require.Len(t, plan.Pages, 2)
	require.NotEqual(t, plan.Pages[0].MemberID, plan.Pages[1].MemberID)
	finalized, err := s.LoadFinalizedProduction(t.Context(), job.SetID, job.Revision)
	require.NoError(t, err)
	secondPage := plan.Pages[1]
	secondArtifact := documentproduction.Artifact{
		ID: "76000000-0000-4000-8000-000000000010", MemberID: secondPage.MemberID,
		MemberOrdinal: secondPage.MemberOrdinal, Page: secondPage.Page,
		Role: documentproduction.ArtifactRoleRedactedPage, Path: "VOL001/synthetic-second-page.png",
		SHA256: productionHash("synthetic second PNG"), Size: 20, MediaType: "image/png",
	}
	require.NoError(t, s.RecordBlob(t.Context(), secondArtifact.SHA256, secondArtifact.Size,
		BlobPhysical{Encoding: "raw", StoredBytes: secondArtifact.Size, Created: true}))
	secondStage, err := production.BuildProductionPageStage(job, plan, finalized.Authority.Prepared.Members[1], secondPage, secondArtifact)
	require.NoError(t, err)
	_, err = s.StageProductionPage(t.Context(), claim, secondStage)
	require.NoError(t, err)
	var first bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &first))
	require.Contains(t, first.String(), `"kind":"production_job_page_stages"`)
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(first.Bytes())))
	var second bytes.Buffer
	require.NoError(t, target.ExportMetadata(t.Context(), &second))
	firstLines, secondLines := bytes.Split(first.Bytes(), []byte{'\n'}), bytes.Split(second.Bytes(), []byte{'\n'})
	require.Len(t, secondLines, len(firstLines))
	for i := range firstLines {
		if !bytes.Equal(firstLines[i], secondLines[i]) {
			at := 0
			for at < min(len(firstLines[i]), len(secondLines[i])) && firstLines[i][at] == secondLines[i][at] {
				at++
			}
			t.Fatalf("metadata row %d changed at byte %d: original=%s restored=%s", i, at,
				firstLines[i][max(0, at-70):min(len(firstLines[i]), at+100)],
				secondLines[i][max(0, at-70):min(len(secondLines[i]), at+100)])
		}
	}
	var members, stages int
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM production_members`).Scan(&members))
	require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM production_job_page_stages`).Scan(&stages))
	require.Equal(t, 2, members)
	require.Equal(t, 2, stages)
	for _, table := range productionLifecycleTables {
		var sourceCount, restoredCount int64
		require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&sourceCount))
		require.NoError(t, target.db.QueryRow(`SELECT COUNT(*) FROM `+table).Scan(&restoredCount))
		require.Positive(t, sourceCount, table)
		require.Equal(t, sourceCount, restoredCount, table)
	}

	lines := bytes.Split(bytes.TrimSpace(first.Bytes()), []byte{'\n'})
	stageIndex := -1
	for i, line := range lines {
		var head struct {
			Type string `json:"type"`
			Kind string `json:"kind"`
		}
		require.NoError(t, json.Unmarshal(line, &head))
		if head.Type == metadataProductionLifecycleType && head.Kind == "production_job_page_stages" {
			stageIndex = i
		}
	}
	require.NotEqual(t, -1, stageIndex)
	importLines := func(t *testing.T, rows [][]byte) error {
		t.Helper()
		fresh := newTestStore(t)
		return fresh.ImportMetadata(t.Context(), bytes.NewReader(append(bytes.Join(rows, []byte{'\n'}), '\n')))
	}
	t.Run("missing row", func(t *testing.T) {
		missing := append([][]byte(nil), lines[:stageIndex]...)
		missing = append(missing, lines[stageIndex+1:]...)
		require.ErrorContains(t, importLines(t, missing), "manifest does not match")
	})
	t.Run("tampered row", func(t *testing.T) {
		var record metadataProductionLifecycle
		require.NoError(t, json.Unmarshal(lines[stageIndex], &record))
		record.Values[4] = "t:tampered"
		raw, err := canonical.Marshal(record)
		require.NoError(t, err)
		changed := append([][]byte(nil), lines...)
		changed[stageIndex] = raw
		require.ErrorContains(t, importLines(t, changed), "invalid production lifecycle checksum")
	})
	t.Run("contradictory row with matching checksums", func(t *testing.T) {
		var otherBlob string
		require.NoError(t, s.db.QueryRow(`SELECT hash FROM blobs WHERE hash<>? ORDER BY hash LIMIT 1`, stage.Artifact.SHA256).Scan(&otherBlob))
		var record metadataProductionLifecycle
		require.NoError(t, json.Unmarshal(lines[stageIndex], &record))
		record.Values[4] = "t:" + otherBlob
		valuesRaw, err := canonical.Marshal(record.Values)
		require.NoError(t, err)
		record.Checksum = digestProductionBytes(valuesRaw)
		changed := append([][]byte(nil), lines...)
		changed[stageIndex], err = canonical.Marshal(record)
		require.NoError(t, err)
		digest := sha256.New()
		for _, table := range productionLifecycleTables {
			writeProductionLifecycleDigest(digest, table, "")
			for _, line := range changed {
				var row metadataProductionLifecycle
				if json.Unmarshal(line, &row) == nil && row.Type == metadataProductionLifecycleType && row.Kind == table {
					writeProductionLifecycleDigest(digest, table, row.Checksum)
				}
			}
		}
		var manifest metadataProductionLifecycleManifest
		require.NoError(t, json.Unmarshal(changed[len(changed)-1], &manifest))
		manifest.Checksum = hex.EncodeToString(digest.Sum(nil))
		changed[len(changed)-1], err = canonical.Marshal(manifest)
		require.NoError(t, err)
		require.Error(t, importLines(t, changed))
	})
}

func TestProductionLifecycleMetadataRequiresManifestEvenWhenEmpty(t *testing.T) {
	source := newTestStore(t)
	var exported bytes.Buffer
	require.NoError(t, source.ExportMetadata(t.Context(), &exported))
	lines := bytes.Split(bytes.TrimSpace(exported.Bytes()), []byte{'\n'})
	require.Contains(t, string(lines[len(lines)-1]), metadataProductionLifecycleManifestType)
	target := newTestStore(t)
	withoutManifest := append(bytes.Join(lines[:len(lines)-1], []byte{'\n'}), '\n')
	require.ErrorContains(t, target.ImportMetadata(t.Context(), bytes.NewReader(withoutManifest)), "manifest is missing")
}
