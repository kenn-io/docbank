package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// subscribeWarm subscribes and runs one empty write outside the timer, so the
// measurement covers steady-state trigger cost rather than the one-time install.
func subscribeWarm(b *testing.B, s *Store) func() {
	b.Helper()
	_, stop := s.SubscribePhotoEvents()
	require.NoError(b, s.withStorageTx(b.Context(), func(*sql.Tx) error { return nil }))
	return stop
}

// BenchmarkPhotoImportMetadata imports a fixed batch into a fresh synthetic
// catalog each iteration. It includes committed metadata transactions, but no
// blob I/O; fixture creation and checkpointing are outside the timer.
func BenchmarkPhotoImportMetadata(b *testing.B) {
	const batch = 32
	type ingestCase struct {
		existing int
		shared   bool
	}
	cases := []ingestCase{{100, false}, {1000, false}, {10000, false}, {1000, true}, {10000, true}}
	if os.Getenv("DOCBANK_INGEST_SCALE") != "" {
		cases = append(cases, ingestCase{100000, false}, ingestCase{100000, true})
	}
	for _, tc := range cases {
		existing := tc.existing
		for _, mode := range []struct{ edit, subscribed bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
			edit, subscribed := mode.edit, mode.subscribed
			if tc.shared && edit {
				continue
			}
			b.Run(fmt.Sprintf("existing=%d/edit=%t/subscriber=%t/shared=%t", existing, edit, subscribed, tc.shared), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					b.StopTimer()
					dir := b.TempDir()
					s, err := Open(b.Context(), filepath.Join(dir, "docbank.db"))
					require.NoError(b, err)
					run, err := s.BeginIngest(b.Context(), "cli", "Synthetic photo corpus")
					require.NoError(b, err)
					var prior []Node
					require.NoError(b, s.withStorageTx(b.Context(), func(tx *sql.Tx) error {
						parentID := s.RootID()
						if tc.shared {
							parent, err := s.mkdirTx(b.Context(), tx, s.RootID(), "elsewhere", nowRFC3339())
							if err != nil {
								return err
							}
							parentID = parent.ID
						}
						for i := range existing {
							name := fmt.Sprintf("photo-%05d.jpg", i)
							hash := fmt.Sprintf("%064x", i+1)
							if tc.shared {
								hash = fakeHash("a1")
							}
							if edit && i >= existing-batch {
								receipt, _, _, err := s.ingestFileTx(b.Context(), tx, run, parentID, name,
									hash, 128, "image/jpeg", "/synthetic/"+name, "", ingestFileOptions{observeMembership: true})
								if err != nil {
									return err
								}
								prior = append(prior, receipt.Node)
							} else {
								if _, _, err := s.createFileTx(b.Context(), tx, parentID, name, hash, 128, "image/jpeg"); err != nil {
									return err
								}
							}
						}
						return nil
					}))
					// A later import records fresh provenance even when the source path is unchanged.
					run, err = s.BeginIngest(b.Context(), "photo-import", "/synthetic")
					require.NoError(b, err)
					groups := make([]PhotoImportGroup, batch)
					for i := range batch {
						name := fmt.Sprintf("photo-%05d.jpg", existing+i)
						if edit {
							name = prior[i].Name
						}
						groups[i] = PhotoImportGroup{Members: []PhotoImportMember{{
							Name: name, Role: PhotoRoleImage, BlobHash: fmt.Sprintf("%064x", existing+i+1),
							Size: 128, MediaType: "image/jpeg", OriginalPath: "/synthetic/" + name,
						}}}
						if tc.shared {
							groups[i].Members[0].BlobHash = fakeHash("a1")
						}
					}
					require.NoError(b, s.Checkpoint(b.Context()))
					stop := func() {}
					if subscribed {
						stop = subscribeWarm(b, s)
					}
					b.StartTimer()
					for i, group := range groups {
						result, err := s.IngestPhotoGroup(b.Context(), run, group)
						if err != nil || !result.Added || len(result.Nodes) != 1 {
							b.Fatalf("import result=%+v err=%v", result, err)
						}
						if result.Nodes[0].BlobHash != group.Members[0].BlobHash {
							b.Fatal("import did not publish the requested content")
						}
						if edit && result.Nodes[0].ID != prior[i].ID {
							b.Fatal("edited source did not retain its node")
						}
					}
					b.StopTimer()
					stop()
					require.NoError(b, s.Close())
					require.NoError(b, os.RemoveAll(dir))
					b.StartTimer()
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*batch), "ns/file")
			})
		}
	}
}

// BenchmarkNonPhotoIngest measures the capture cost that document writes pay while a photo
// stream is open. An ingest writes nodes, which the triggers watch, so each commit also publishes.
func BenchmarkNonPhotoIngest(b *testing.B) {
	const files = 1000
	for _, subscribed := range []bool{false, true} {
		b.Run(fmt.Sprintf("subscriber=%t", subscribed), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				dir := b.TempDir()
				s, err := Open(b.Context(), filepath.Join(dir, "docbank.db"))
				require.NoError(b, err)
				run, err := s.BeginIngest(b.Context(), "cli", "Synthetic documents")
				require.NoError(b, err)
				require.NoError(b, s.Checkpoint(b.Context()))
				stop := func() {}
				if subscribed {
					stop = subscribeWarm(b, s)
				}
				b.StartTimer()
				for i := range files {
					name := fmt.Sprintf("note-%05d.txt", i)
					_, _, err := s.IngestFile(b.Context(), run, s.RootID(), name,
						fmt.Sprintf("%064x", i+1), 4, "text/plain", "/synthetic/"+name, "")
					require.NoError(b, err)
				}
				b.StopTimer()
				stop()
				require.NoError(b, s.Close())
				require.NoError(b, os.RemoveAll(dir))
				b.StartTimer()
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*files), "ns/file")
		})
	}
}
