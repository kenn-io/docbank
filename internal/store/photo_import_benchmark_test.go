package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkPhotoImportMetadata imports a fixed batch into a fresh synthetic
// catalog each iteration. It includes committed metadata transactions, but no
// blob I/O; fixture creation and checkpointing are outside the timer.
func BenchmarkPhotoImportMetadata(b *testing.B) {
	const batch = 32
	for _, existing := range []int{100, 10000, 100000} {
		for _, edit := range []bool{false, true} {
			for _, audience := range []string{"none", "locked", "hidden"} {
				b.Run(fmt.Sprintf("existing=%d/edit=%t/subscriber=%s", existing, edit, audience), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						b.StopTimer()
						dir := b.TempDir()
						s, err := Open(filepath.Join(dir, "docbank.db"))
						require.NoError(b, err)
						run, err := s.BeginIngest(b.Context(), "cli", "Synthetic photo corpus")
						require.NoError(b, err)
						var prior []Node
						require.NoError(b, s.withStorageTx(b.Context(), func(tx *sql.Tx) error {
							for i := range existing {
								name := fmt.Sprintf("photo-%05d.jpg", i)
								receipt, _, _, err := s.ingestFileTx(b.Context(), tx, run, s.RootID(), name,
									fmt.Sprintf("%064x", i+1), 128, "image/jpeg", "/synthetic/"+name, "",
									ingestFileOptions{observeMembership: true})
								if err != nil {
									return err
								}
								if i >= existing-batch {
									prior = append(prior, receipt.Node)
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
						}
						ctx, cancel := setupPhotoBenchmarkCapture(b, s, audience)
						require.NoError(b, s.Checkpoint(ctx))
						b.StartTimer()
						for i, group := range groups {
							result, err := s.IngestPhotoGroup(ctx, run, group)
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
						cancel()
						require.NoError(b, s.Close())
						require.NoError(b, os.RemoveAll(dir))
						b.StartTimer()
					}
					b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*batch), "ns/file")
				})
			}
		}
	}
}

// Both workloads create the same tag before timing.
func setupPhotoBenchmarkCapture(b *testing.B, s *Store, audience string) (context.Context, func()) {
	b.Helper()
	ctx := b.Context()
	if audience == "hidden" {
		require.NoError(b, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
		token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
		require.NoError(b, err)
		ctx = WithPhotoHiddenToken(ctx, token)
	}
	var events <-chan PhotoEvent
	cancel := func() {}
	if audience != "none" {
		events, cancel = s.SubscribePhotoEvents(ctx)
	}
	_, err := s.CreateTag(ctx, "Synthetic benchmark keyword")
	require.NoError(b, err)
	if events != nil {
		select {
		case event := <-events:
			require.Equal(b, PhotoEvent{Refetch: true, audience: photoPublic}, event)
		default:
			b.Fatal("creating a tag did not publish a capture event")
		}
	}
	return ctx, cancel
}
