package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Include both a superseded text version and a terminal extraction failure.
// Neither should make a settled catalog rebuild its successful text on open.
func BenchmarkSettledLegacyMigration(b *testing.B) {
	for _, files := range []int{100, 1000} {
		b.Run(fmt.Sprintf("files=%d", files), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			ctx := b.Context()
			text := strings.Repeat("searchable migration benchmark ", 256)
			var last Node
			for i := range files {
				hash := fmt.Sprintf("%064x", i+1)
				last, err = s.CreateFile(ctx, s.RootID(), fmt.Sprintf("text-%04d.txt", i),
					hash, int64(len(text)), "text/plain")
				require.NoError(b, err)
				require.NoError(b, s.RecordExtraction(ctx, ExtractionResult{
					BlobHash: hash, Extractor: "plain-text", ExtractorVersion: 1,
					Status: ExtractionOK, Text: text,
				}))
			}
			_, _, err = s.ReplaceContent(ctx, last.ID, last.Revision,
				fmt.Sprintf("%064x", files+1), 1, "application/octet-stream")
			require.NoError(b, err)
			_, err = s.MigrateLegacyPlainText(ctx)
			require.NoError(b, err)
			failed, err := s.CreateFile(ctx, s.RootID(), "failed.txt",
				fmt.Sprintf("%064x", files+2), 1, "text/plain")
			require.NoError(b, err)
			require.NoError(b, s.RecordExtraction(ctx, ExtractionResult{
				BlobHash: failed.BlobHash, Extractor: "plain-text", ExtractorVersion: 1,
				Status: ExtractionFailed, Error: "synthetic terminal failure",
			}))
			b.ReportAllocs()
			for b.Loop() {
				_, err := s.MigrateLegacyPlainText(ctx)
				require.NoError(b, err)
			}
		})
	}
}
