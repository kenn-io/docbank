package store

import (
	"database/sql"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQuerySnapshotScale records the required large synthetic projection
// evidence without making ordinary focused runs pay the fixture cost.
func TestQuerySnapshotScale(t *testing.T) {
	if os.Getenv("DOCBANK_QUERY_SNAPSHOT_SCALE") != "1" {
		t.Skip("set DOCBANK_QUERY_SNAPSHOT_SCALE=1 for the 25k/118k snapshot measurements")
	}
	s := newTestStore(t)
	run, err := s.BeginIngest(t.Context(), "cli", "Synthetic snapshot scale")
	require.NoError(t, err)
	const sourceCount = 1024
	hashes := make([]string, sourceCount)
	for i := range hashes {
		hashes[i] = testSHA256([]byte(fmt.Sprintf("synthetic-snapshot-source-%04d", i)))
	}
	value := snapshotTestQuery(t, `{}`)
	seeded := 0
	t.Logf("host go=%s os=%s arch=%s cpus=%d sqlite_driver=%T", runtime.Version(), runtime.GOOS,
		runtime.GOARCH, runtime.NumCPU(), s.driver)
	t.Logf("fixture source_count=%d mime_type=text/plain batch_size=1000 source_size_min=100 source_size_max=%d generation=native provenance=canonical",
		sourceCount, 100+sourceCount-1)
	for _, target := range []int{25000, 118000} {
		added := target - seeded
		seedStarted := time.Now()
		func() {
			stopProfile := startQuerySnapshotSetupProfile(t, target)
			defer stopProfile()
			for seeded < target {
				batchEnd := min(seeded+1000, target)
				require.NoError(t, s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
					if _, err := ensureIngestRunTx(t.Context(), tx, run); err != nil {
						return err
					}
					for index := seeded; index < batchEnd; index++ {
						name := fmt.Sprintf("synthetic-snapshot-%06d.txt", index)
						source := index % sourceCount
						hash := hashes[source]
						node, _, err := s.createFileTx(t.Context(), tx, s.RootID(), name, hash,
							int64(100+source), "text/plain")
						if err != nil {
							return err
						}
						fact := metadataProvenance{
							Type: metadataProvenanceType, NodeID: node.ID, IngestID: run.ID(),
							OriginalPath: "/synthetic/" + name,
						}
						identity, err := provenanceIdentity(fact)
						if err != nil {
							return err
						}
						_, err = tx.ExecContext(t.Context(), `INSERT INTO provenance(
							identity,node_id,ingest_id,original_path
						) VALUES(?,?,?,?)`, identity, node.ID, run.ID(), fact.OriginalPath)
						if err != nil {
							return err
						}
					}
					return nil
				}))
				seeded = batchEnd
				if seeded%10000 == 0 || seeded == target {
					t.Logf("seed_progress target=%d current_members=%d stage_elapsed=%s",
						target, seeded, time.Since(seedStarted))
				}
			}
		}()
		t.Logf("seed target=%d added=%d elapsed=%s", target, added, time.Since(seedStarted))
		for _, phase := range []string{"first_after_seed", "repeat"} {
			started := time.Now()
			projection, err := s.MaterializeQuerySnapshot(t.Context(), SnapshotRequest{Query: value})
			require.NoError(t, err)
			assert.Equal(t, int64(target), projection.Total)
			assert.Len(t, projection.MemberHash, 64)
			assert.Positive(t, projection.SerializedBytes)
			assert.LessOrEqual(t, projection.SerializedBytes, querySnapshotMaxBytes)
			t.Logf("materialize target=%d phase=%s elapsed=%s total_bytes=%d serialized_bytes=%d generation=%s coverage=%s",
				target, phase, time.Since(started), projection.TotalBytes, projection.SerializedBytes,
				projection.Generation.Kind, projection.Coverage.Configuration)
		}
		started := time.Now()
		faceted, err := s.MaterializeQuerySnapshot(t.Context(), SnapshotRequest{
			Query: value,
			Facets: []string{
				"collections", "tags", "media_family", "extension", "modified", "size",
				"text_coverage", "duplicates",
			},
		})
		require.NoError(t, err)
		available := 0
		reasons := make(map[string]int)
		for _, facet := range faceted.Facets {
			if facet.Available {
				available++
			} else {
				reasons[facet.Reason]++
			}
		}
		t.Logf("materialize target=%d phase=all_facets elapsed=%s serialized_bytes=%d facets_available=%d facet_unavailable=%v",
			target, time.Since(started), faceted.SerializedBytes, available, reasons)

		func() {
			service := NewQuerySnapshotService(s)
			defer func() { require.NoError(t, service.Close()) }()
			started := time.Now()
			first, err := service.Create(t.Context(), "synthetic-scale-owner", SnapshotRequest{
				Query: value, PageSize: 250,
			})
			require.NoError(t, err)
			require.Equal(t, int64(target), first.Total)
			require.Len(t, first.Rows, 250)
			require.NotEmpty(t, first.NextCursor)
			stats := service.stats()
			t.Logf("cache_create target=%d elapsed=%s serialized_bytes=%d admitted_bytes=%d cached_rows=%d page_rows=%d",
				target, time.Since(started), first.SerializedBytes, stats.CachedBytes,
				stats.CachedRows, len(first.Rows))

			started = time.Now()
			next, err := service.Page(t.Context(), "synthetic-scale-owner", first.SnapshotID, first.NextCursor)
			require.NoError(t, err)
			require.Len(t, next.Rows, 250)
			require.NotEmpty(t, next.PrevCursor)
			t.Logf("cache_page target=%d elapsed=%s page_rows=%d previous_cursor=%t next_cursor=%t",
				target, time.Since(started), len(next.Rows), next.PrevCursor != "", next.NextCursor != "")
		}()
	}
}

func startQuerySnapshotSetupProfile(t *testing.T, target int) func() {
	t.Helper()
	prefix := os.Getenv("DOCBANK_QUERY_SNAPSHOT_SETUP_PROFILE")
	if prefix == "" {
		return func() {}
	}
	path := fmt.Sprintf("%s-%d.pprof", prefix, target)
	file, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, pprof.StartCPUProfile(file))
	started := time.Now()
	return func() {
		pprof.StopCPUProfile()
		require.NoError(t, file.Close())
		t.Logf("seed_profile target=%d elapsed=%s path=%s", target, time.Since(started), path)
	}
}
