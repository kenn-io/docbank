package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/canonical"
)

// BenchmarkMediaOperationHistory includes complete source reads and committed
// occurrence declarations. Each declaration adds one receipt to the history.
func BenchmarkMediaOperationHistory(b *testing.B) {
	for _, shape := range []struct {
		history    int
		sameSource bool
	}{{0, false}, {1000, false}, {10000, false}, {1000, true}, {10000, true}} {
		b.Run(fmt.Sprintf("history=%d/same_source=%t", shape.history, shape.sameSource), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			ctx := b.Context()
			const principal = "operator:benchmark"
			sourceID, otherID := fmt.Sprintf("%064x", 1), fmt.Sprintf("%064x", 2)
			occurrence := MediaOccurrenceInput{ID: "selected", SourceID: sourceID,
				Principal: principal, Ref: "selected", Revision: "1", MessageJSON: "{}"}
			retained, err := s.RetainMediaReference(ctx, MediaReferencePublicationRequest{
				Operation: MediaOperation{ID: "00000000-0000-4000-8000-000000000001", Principal: principal,
					Verb: "submit_remote_recording", RequestSHA256: sourceID, SourceID: sourceID},
				Provider: "url", OriginScope: sourceID, IdentitySHA256: sourceID,
				Outcome: "unsupported", Occurrence: occurrence,
			})
			require.NoError(b, err)
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `INSERT INTO media_sources VALUES(?,?,?,?,?,?)`,
					otherID, "remote_recording", "url", otherID, otherID, "2026-01-01T00:00:00Z")
				if err != nil {
					return err
				}
				historySource := otherID
				if shape.sameSource {
					historySource = sourceID
				}
				stmt, err := tx.PrepareContext(ctx, `INSERT INTO media_operations VALUES(?,?,?,?,?,?,?,?)`)
				if err != nil {
					return err
				}
				defer func() { _ = stmt.Close() }()
				for i := range shape.history {
					id := fmt.Sprintf("11111111-1111-4111-8111-%012x", i+1)
					raw, err := canonical.Marshal(MediaPublicationReceipt{
						VaultUID: s.VaultID(), SourceID: historySource, OperationID: id,
						OperationState: "succeeded", CoverageState: "unprocessed",
					})
					if err != nil {
						return err
					}
					_, err = stmt.ExecContext(ctx, id, principal, "declare_occurrence", sourceID,
						historySource, string(raw), "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
					if err != nil {
						return err
					}
				}
				return nil
			}))
			// Keep the fixture's WAL checkpoint outside the measured declarations.
			require.NoError(b, s.Checkpoint(ctx))
			b.Run("list", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					items, total, more, err := s.MediaSources(ctx, principal, "", 25)
					if err != nil || total != 1 || more || len(items) != 1 {
						b.Fatalf("items=%d total=%d more=%t err=%v", len(items), total, more, err)
					}
					if items[0].SourceID != sourceID || items[0].Receipt.OperationID != retained.OperationID {
						b.Fatal("wrong source or receipt")
					}
				}
			})
			b.Run("declare", func(b *testing.B) {
				b.ReportAllocs()
				sequence := 0
				for b.Loop() {
					sequence++
					id := fmt.Sprintf("22222222-2222-4222-8222-%012x", sequence)
					receipt, err := s.RecordMediaOccurrence(ctx, MediaOperation{ID: id, Principal: principal,
						Verb: "declare_occurrence", RequestSHA256: sourceID, SourceID: sourceID}, occurrence)
					if err != nil || receipt.OperationID != id || receipt.OccurrenceID != occurrence.ID {
						b.Fatalf("unexpected declaration receipt: %+v err=%v", receipt, err)
					}
				}
			})
		})
	}
}
