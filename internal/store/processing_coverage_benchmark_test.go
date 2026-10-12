package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkProcessingCoverage measures a complete coverage snapshot, excluding
// creation of synthetic sources and their retained rendition builds.
func BenchmarkProcessingCoverage(b *testing.B) {
	for _, tc := range []struct{ versions, builds int }{{1, 0}, {1, 1}, {100, 0}, {100, 1}, {100, 100}} {
		b.Run(fmt.Sprintf("versions=%d/builds=%d", tc.versions, tc.builds), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			profile := catalogProcessingProfile(b, false)
			var versions []string
			require.NoError(b, s.withStorageTx(b.Context(), func(tx *sql.Tx) error {
				for hash, content := range catalogBlobContents {
					if err := s.EnsureBlobTx(tx, hash, int64(len(content))); err != nil {
						return err
					}
				}
				for i := range tc.versions {
					node, _, err := s.createFileTx(b.Context(), tx, s.RootID(),
						fmt.Sprintf("source-%03d.pdf", i), catalogSourceHash, 20, "application/pdf")
					if err != nil {
						return err
					}
					versions = append(versions, node.CurrentVersionID)
				}
				return nil
			}))
			for i := range tc.builds {
				build := catalogRenditionBuild(s, profile)
				build.ID = fmt.Sprintf("%064x", i+1)
				require.NoError(b, s.StageRenditionBuild(b.Context(), build))
			}
			generation, err := s.StageLexicalGeneration(b.Context(), fakeHash("c1"))
			require.NoError(b, err)
			for i, version := range versions {
				if tc.builds == 0 {
					break
				}
				attachment := RenditionAttachmentRecord{
					ID: fmt.Sprintf("%064x", i+1), VaultID: s.VaultID(), ContentVersionID: version,
					BuildID: fmt.Sprintf("%064x", i%tc.builds+1), Profile: profile, AttachedAt: nowRFC3339(),
				}
				require.NoError(b, s.PublishRenditionAndLexicalHeads(b.Context(), attachment, RenditionHeadRecord{
					ContentVersionID: version, ProcessingProfileFingerprint: profile.Fingerprint,
					AttachmentID: attachment.ID, PublishedAt: nowRFC3339(),
				}, generation.ID))
			}
			scope := ProcessingCoverageScope{ContentVersionIDs: versions, ProcessingProfileFingerprint: profile.Fingerprint}
			want := ProcessingClassCoverage{Name: "rendition", State: "complete", Complete: tc.versions, Total: tc.versions}
			if tc.builds == 0 {
				want.State, want.Complete, want.Unavailable = "unavailable", 0, tc.versions
			}
			b.ReportAllocs()
			for b.Loop() {
				coverage, err := s.ProcessingCoverage(b.Context(), scope)
				if err != nil || coverage.Renditions != want || len(coverage.Embeddings) != 0 {
					b.Fatalf("coverage=%+v, want %+v, err=%v", coverage, want, err)
				}
			}
		})
	}
}
