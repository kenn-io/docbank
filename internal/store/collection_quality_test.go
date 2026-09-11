package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Ignoring current-version metadata, using per-fact membership, or scoping
// duplicates to this collection would change these hand-counted buckets.
func TestCollectionQualityCensusAndFreshFingerprint(t *testing.T) {
	s, run, nodes, profile := collectionCoverageFixture(t, 1)
	_, err := s.IngestFileExact(t.Context(), run, s.RootID(), "image.PDF", fakeHash("d7"), 0, "image/png", "image.PDF", "")
	require.NoError(t, err)
	_, err = s.IngestFileExact(t.Context(), run, s.RootID(), "noextension", fakeHash("d8"), 1024, "", "noextension", "")
	require.NoError(t, err)
	_, err = s.CreateFile(t.Context(), s.RootID(), "outside.pdf", nodes[0].BlobHash, nodes[0].Size, nodes[0].MimeType)
	require.NoError(t, err)
	addCollectionMembership(t, s, run, nodes[0].ID, "repeated fact", nil)
	selection := CoverageSelection{"configured", profile.Fingerprint}
	got, err := s.CollectionQuality(t.Context(), run.ID(), selection, []string{"extension", "duplicates", "size", "text_coverage"})
	require.NoError(t, err)
	require.Equal(t, int64(3), got.Collection.FileCount)
	require.Equal(t, int64(1), got.ZeroBytes)
	require.Equal(t, int64(1), got.Mismatches)
	require.Equal(t, int64(1), got.DuplicateDocuments)
	require.Equal(t, QualityDimension{Field: "extension", Values: []QualityBucket{{"pdf", 2}}, Missing: 1}, qualityDimension(t, got, "extension"))
	require.Equal(t, []QualityBucket{{"unique", 2}, {"duplicate", 1}}, qualityDimension(t, got, "duplicates").Values)
	require.Equal(t, []QualityBucket{{"unprocessed", 3}}, qualityDimension(t, got, "text_coverage").Values)
	require.Len(t, got.SourceFingerprint, 64)
	again, err := s.CollectionQuality(t.Context(), run.ID(), selection, []string{"size", "extension", "text_coverage", "duplicates"})
	require.NoError(t, err)
	require.Equal(t, got.SourceFingerprint, again.SourceFingerprint)
	_, _, err = s.Move(t.Context(), nodes[0].ID, s.RootID(), "changed.txt", nodes[0].Revision)
	require.NoError(t, err)
	changed, err := s.CollectionQuality(t.Context(), run.ID(), selection, []string{"extension"})
	require.NoError(t, err)
	require.NotEqual(t, got.SourceFingerprint, changed.SourceFingerprint)
}

func TestCollectionQualityTopFiftyAndConcentrations(t *testing.T) {
	s, run, _, _ := collectionCoverageFixture(t, 0)
	for i := range 52 {
		name := fmt.Sprintf("document.e%02d", i)
		_, err := s.IngestFileExact(t.Context(), run, s.RootID(), name, testSHA256([]byte(name)), 10, "text/plain", name, "")
		require.NoError(t, err)
	}
	got, err := s.CollectionQuality(t.Context(), run.ID(), CoverageSelection{}, []string{"extension", "media_type"})
	require.NoError(t, err)
	ext := qualityDimension(t, got, "extension")
	require.Len(t, ext.Values, 50)
	require.Equal(t, QualityBucket{"e00", 1}, ext.Values[0])
	require.Equal(t, QualityBucket{"e49", 1}, ext.Values[49])
	require.Equal(t, int64(2), ext.Other)
	require.Equal(t, []QualitySpike{{Field: "media_type", Value: "text/plain", Count: 52}}, got.Spikes)
}

func TestCollectionQualityRejectsInvalidDimensionsAndCancellation(t *testing.T) {
	s, run, _, _ := collectionCoverageFixture(t, 1)
	for _, fields := range [][]string{{"unknown"}, {"extension", "extension"}} {
		_, err := s.CollectionQuality(t.Context(), run.ID(), CoverageSelection{}, fields)
		require.ErrorIs(t, err, ErrInvalidQualityFields)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := s.CollectionQuality(ctx, run.ID(), CoverageSelection{}, nil)
	require.ErrorIs(t, err, ErrQualityUnavailable)
	require.ErrorIs(t, err, context.Canceled)
}

func TestCollectionQualitySizeBoundariesAndUTCMonth(t *testing.T) {
	s, run, _, _ := collectionCoverageFixture(t, 0)
	sizes := []int64{0, 1, 1023, 1024, 1<<20 - 1, 1 << 20, 10<<20 - 1, 10 << 20, 100<<20 - 1, 100 << 20, 1<<30 - 1, 1 << 30}
	for i, size := range sizes {
		name := fmt.Sprintf("size-%d.pdf", i)
		node, err := s.IngestFileExact(t.Context(), run, s.RootID(), name, testSHA256([]byte(name)), size, "application/octet-stream", name, "")
		require.NoError(t, err)
		_, err = s.db.Exec(`UPDATE nodes SET modified_at=? WHERE id=?`, "2026-03-01T00:30:00+02:00", node.ID)
		require.NoError(t, err)
	}
	got, err := s.CollectionQuality(t.Context(), run.ID(), CoverageSelection{}, []string{"size", "modified_month", "media_family", "text_coverage"})
	require.NoError(t, err)
	require.Equal(t, []QualityBucket{{"2026-02", 12}}, qualityDimension(t, got, "modified_month").Values)
	require.Equal(t, []QualityBucket{{"(0,1KiB)", 2}, {"[100MiB,1GiB)", 2}, {"[10MiB,100MiB)", 2}, {"[1KiB,1MiB)", 2}, {"[1MiB,10MiB)", 2}, {">=1GiB", 1}, {"zero", 1}}, qualityDimension(t, got, "size").Values)
	require.Equal(t, []QualityBucket{{"document", 12}}, qualityDimension(t, got, "media_family").Values)
	require.Zero(t, got.Mismatches, "generic MIME is not a mismatch")
	require.Equal(t, int64(12), qualityDimension(t, got, "text_coverage").Missing)
	require.Equal(t, []QualitySpike{{"media_family", "document", 12}, {"modified_month", "2026-02", 12}}, got.Spikes)
}

func TestCollectionQualityConcentrationNeedsTenAndEightyPercent(t *testing.T) {
	for _, tc := range []struct {
		name            string
		matching, total int
		spike           bool
	}{
		{"nine of nine", 9, 9, false}, {"ten of thirteen", 10, 13, false}, {"twelve of fifteen", 12, 15, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, run, _, _ := collectionCoverageFixture(t, 0)
			for i := range tc.total {
				name := fmt.Sprintf("document-%d", i)
				mediaType := ""
				if i < tc.matching {
					mediaType = "text/plain"
				}
				_, err := s.IngestFileExact(t.Context(), run, s.RootID(), name, testSHA256([]byte(name)), 1, mediaType, name, "")
				require.NoError(t, err)
			}
			got, err := s.CollectionQuality(t.Context(), run.ID(), CoverageSelection{}, []string{"media_type"})
			require.NoError(t, err)
			if tc.spike {
				require.Equal(t, []QualitySpike{{"media_type", "text/plain", 12}}, got.Spikes)
			} else {
				require.Empty(t, got.Spikes)
			}
		})
	}
}

func TestCollectionQualityBoundsRejectWholeCensus(t *testing.T) {
	for _, tc := range []struct {
		members, bytes int64
		tooLarge       bool
	}{
		{250000, 64 << 20, false}, {250001, 0, true}, {1, 64<<20 + 1, true},
	} {
		err := validateCollectionQualityBounds(tc.members, tc.bytes)
		if tc.tooLarge {
			require.ErrorIs(t, err, ErrQualityTooLarge)
		} else {
			require.NoError(t, err)
		}
	}
	s, run, _, _ := collectionCoverageFixture(t, 0)
	// The real store allows arbitrary UTF-8 MIME metadata. One oversized value
	// must be refused by the SQL projection preflight even for extension-only reads.
	_, err := s.IngestFileExact(t.Context(), run, s.RootID(), "oversized.pdf", fakeHash("cc"), 1,
		strings.Repeat("a", 64<<20), "oversized.pdf", "")
	require.NoError(t, err)
	got, err := s.CollectionQuality(t.Context(), run.ID(), CoverageSelection{}, []string{"extension"})
	require.ErrorIs(t, err, ErrQualityTooLarge)
	require.Empty(t, got.SourceFingerprint)
	require.Empty(t, got.Dimensions)
}

func TestCollectionQualityCensusKeepsMetadataCoverageAndGenerationInOneSnapshot(t *testing.T) {
	s, run, nodes, profile := collectionCoverageFixture(t, 1)
	selection := CoverageSelection{"configured", profile.Fingerprint}
	collectionCoveragePublish(t, s, nodes[0], profile, "complete")
	before, err := s.CollectionQuality(t.Context(), run.ID(), selection, []string{"extension", "text_coverage"})
	require.NoError(t, err)
	var snapshot CollectionQuality
	err = s.withLexicalGenerationRead(t.Context(), func(q metadataQuerier, _ LexicalGeneration) error {
		_, err := collectionSummaryByID(t.Context(), q, run.ID())
		if err != nil {
			return err
		}
		_, _, err = s.ReplaceContent(t.Context(), nodes[0].ID, nodes[0].Revision, fakeHash("bc"), 4, "text/plain")
		if err != nil {
			return err
		}
		census, err := collectionQualityCensusTx(t.Context(), q, run.ID(), selection)
		if err != nil {
			return err
		}
		snapshot, err = aggregateCollectionQuality(t.Context(), census, []string{"extension", "text_coverage"})
		return err
	})
	require.NoError(t, err)
	require.Equal(t, before, snapshot)
	after, err := s.CollectionQuality(t.Context(), run.ID(), selection, []string{"extension", "text_coverage"})
	require.NoError(t, err)
	require.Equal(t, CoverageCounts{Unprocessed: 1}, *after.Collection.Coverage.Counts)
	require.Equal(t, int64(4), after.Collection.TotalBytes)
	require.NotEqual(t, before.SourceFingerprint, after.SourceFingerprint)
}

func qualityDimension(t *testing.T, quality CollectionQuality, field string) QualityDimension {
	t.Helper()
	for _, dimension := range quality.Dimensions {
		if dimension.Field == field {
			return dimension
		}
	}
	t.Fatalf("missing dimension %s", field)
	return QualityDimension{}
}

// Opt in explicitly: this proof creates 250001 real current versions and is
// intentionally excluded from routine test runs. All files and text are synthetic.
func TestCollectionQualityScaleProof(t *testing.T) {
	if os.Getenv("DOCBANK_COLLECTION_QUALITY_SCALE") != "1" {
		t.Skip("set DOCBANK_COLLECTION_QUALITY_SCALE=1 for the large synthetic census proof")
	}
	s, run, _, profile := collectionCoverageFixture(t, 0)
	selection := CoverageSelection{"configured", profile.Fingerprint}
	const sourceCount = 1024
	texts := make([]string, sourceCount)
	hashes := make([]string, sourceCount)
	for i := range texts {
		texts[i] = fmt.Sprintf("Synthetic census text source %04d. Shared native searchable evidence.", i)
		hashes[i] = testSHA256([]byte(texts[i]))
	}
	seeded := 0
	for _, target := range []int{25000, 118000, 250001} {
		seedStarted := time.Now()
		for seeded < target {
			batchEnd := min(seeded+1000, target)
			require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
				if _, err := ensureIngestRunTx(t.Context(), tx, run); err != nil {
					return err
				}
				for index := seeded; index < batchEnd; index++ {
					name := fmt.Sprintf("synthetic-census-%06d.txt", index)
					source := index % sourceCount
					node, _, err := s.createFileTx(t.Context(), tx, s.RootID(), name, hashes[source], int64(len(texts[source])), "text/plain")
					if err != nil {
						return err
					}
					fact := metadataProvenance{Type: metadataProvenanceType, NodeID: node.ID, IngestID: run.ID(), OriginalPath: name}
					identity, err := provenanceIdentity(fact)
					if err != nil {
						return err
					}
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO provenance(identity,node_id,ingest_id,original_path) VALUES(?,?,?,?)`, identity, node.ID, run.ID(), name); err != nil {
						return err
					}
				}
				return nil
			}))
			seeded = batchEnd
			if seeded%25000 == 0 {
				t.Logf("seed progress current_members=%d target=%d stage_elapsed=%s", seeded, target, time.Since(seedStarted))
			}
		}
		t.Logf("seed current_members=%d elapsed=%s", target, time.Since(seedStarted))
		if target == 25000 {
			for i, hash := range hashes {
				require.NoError(t, s.RecordExtraction(t.Context(), ExtractionResult{BlobHash: hash, Extractor: "synthetic-scale-native", ExtractorVersion: 1, Status: ExtractionOK, Text: texts[i]}))
			}
		}
		service := NewCollectionQualityService(s)
		started := time.Now()
		cold, err := service.Read(t.Context(), run.ID(), selection, nil)
		coldElapsed := time.Since(started)
		if target > 250000 {
			require.ErrorIs(t, err, ErrQualityTooLarge)
			require.Empty(t, cold.SourceFingerprint)
			t.Logf("rejected current_members=%d elapsed=%s error=%v", target, coldElapsed, err)
			continue
		}
		if err != nil {
			require.ErrorIs(t, err, ErrQualityUnavailable)
			t.Logf("bounded unavailable current_members=%d cold=%s error=%v", target, coldElapsed, err)
			continue
		}
		require.Equal(t, int64(target), cold.Collection.FileCount)
		require.Equal(t, CoverageCounts{Complete: int64(target)}, *cold.Collection.Coverage.Counts)
		require.Equal(t, int64(target), cold.DuplicateDocuments)
		started = time.Now()
		warm, err := service.Read(t.Context(), run.ID(), selection, nil)
		warmElapsed := time.Since(started)
		if err != nil {
			require.ErrorIs(t, err, ErrQualityUnavailable)
			t.Logf("bounded unavailable current_members=%d cold=%s warm=%s error=%v", target, coldElapsed, warmElapsed, err)
			continue
		}
		require.Equal(t, cold, warm)
		t.Logf("census current_members=%d native_sources=%d complete=%d cold=%s warm=%s fingerprint=%s", target, sourceCount, cold.Collection.Coverage.Counts.Complete, coldElapsed, warmElapsed, cold.SourceFingerprint)
	}
}
