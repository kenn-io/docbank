package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/canonical"
)

// BenchmarkMediaSourcePages includes totals, newest-occurrence selection, and
// receipt decoding. Synthetic fixture creation is outside the timer.
func BenchmarkMediaSourcePages(b *testing.B) {
	for _, shape := range []struct{ sources, occurrences int }{
		{1, 1}, {1, 100}, {1, 1000}, {100, 1}, {100, 100},
	} {
		b.Run(fmt.Sprintf("sources=%d/occurrences=%d", shape.sources, shape.occurrences), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			ctx := b.Context()
			const principal = "operator:benchmark"
			const stamp = "2026-01-01T00:00:00Z"
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				for source := range shape.sources {
					id := fmt.Sprintf("%064x", source+1)
					if _, err := tx.ExecContext(ctx, `INSERT INTO media_sources VALUES(?,?,?,?,?,?)`,
						id, "remote_recording", "url", "synthetic", id, stamp); err != nil {
						return err
					}
					operation := fmt.Sprintf("operation-%d", source)
					receipt, err := canonical.Marshal(MediaPublicationReceipt{
						VaultUID: s.VaultID(), SourceID: id, OccurrenceID: fmt.Sprintf("%d-0000", source),
						OperationID: operation, OperationState: "succeeded", CoverageState: "unprocessed",
						Outcome: "unsupported",
					})
					if err != nil {
						return err
					}
					if _, err := tx.ExecContext(ctx, `INSERT INTO media_operations VALUES(?,?,?,?,?,?,?,?)`,
						operation, principal, "submit_remote_recording", id, id, string(receipt), stamp, stamp); err != nil {
						return err
					}
					for occurrence := range shape.occurrences {
						occurrenceID := fmt.Sprintf("%d-%04d", source, occurrence)
						if _, err := tx.ExecContext(ctx, `INSERT INTO media_occurrences VALUES(?,?,NULL,?,?,?,'recording.wav','','','{}',1,?,NULL)`,
							occurrenceID, id, principal, occurrenceID, "1", stamp); err != nil {
							return err
						}
					}
				}
				return nil
			}))
			starts := []int{0}
			if shape.sources > 1 {
				starts = append(starts, shape.sources/2)
			}
			for _, start := range starts {
				b.Run(fmt.Sprintf("after=%d", start), func(b *testing.B) {
					after := ""
					if start > 0 {
						after = fmt.Sprintf("%064x", start)
					}
					b.ReportAllocs()
					for b.Loop() {
						items, total, more, err := s.MediaSources(ctx, principal, after, 25)
						if err != nil || total != shape.sources || len(items) != min(shape.sources-start, 25) || more != (shape.sources-start > 25) {
							b.Fatalf("items=%d total=%d more=%t err=%v", len(items), total, more, err)
						}
						for i, item := range items {
							source := start + i
							if item.SourceID != fmt.Sprintf("%064x", source+1) || item.OccurrenceID != fmt.Sprintf("%d-%04d", source, shape.occurrences-1) || item.Receipt.OperationID != fmt.Sprintf("operation-%d", source) {
								b.Fatal("unexpected source, newest occurrence, or receipt")
							}
						}
					}
				})
			}
		})
	}
}
