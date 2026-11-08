package processing

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/notemarkdown"
	"go.kenn.io/docbank/internal/store"
)

func TestNotePlaintextBackfillRequiresConsentAndRecoversReappearance(t *testing.T) {
	t.Parallel()
	f := newNotePlaintextFixture(t)
	const body = "# Synthetic\nRecoveryword evidence\n"
	node := f.note(t, "recover.md", body)
	note, err := notemarkdown.Parse(t.Context(), []byte(body))
	require.NoError(t, err)
	artifact, err := notemarkdown.CanonicalArtifact(t.Context(), note)
	require.NoError(t, err)
	sum := sha256.Sum256(artifact)
	require.NoError(t, f.catalog.RecordNoteParse(t.Context(), store.NoteParseRecord{
		ContentVersionID: node.CurrentVersionID, Hash: node.BlobHash, Size: node.Size,
		Recipe: notemarkdown.Recipe, State: "ready", Checksum: hex.EncodeToString(sum[:]), Artifact: artifact,
	}))
	backfill, err := f.service.NewNotePlaintextBackfill(slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	backfill.DrainOnce = true
	require.NoError(t, backfill.Run(t.Context()))
	processed, err := f.worker.RunOne(t.Context())
	require.NoError(t, err)
	require.False(t, processed, "preview alone must not grant processing")
	_, consent, err := f.service.NoteLocalTextPlan(t.Context())
	require.NoError(t, err)
	_, err = f.catalog.GrantProcessingConsent(t.Context(), consent)
	require.NoError(t, err)
	require.NoError(t, backfill.Run(t.Context()))
	before := f.catalog.WriteRevision()
	require.NoError(t, backfill.Run(t.Context()))
	require.Equal(t, before, f.catalog.WriteRevision(), "admitted work must not hot-loop")

	begin := func() store.NotePublicationScan {
		scan, err := f.catalog.BeginNotePublicationScan(t.Context(), store.NotePublicationBeginRequest{
			Name: "notes", Destination: "/", PolicyFingerprint: strings.Repeat("a", 64), OperationID: uuid.NewV4().String(),
		})
		require.NoError(t, err)
		return scan
	}
	complete := func(scan store.NotePublicationScan) {
		_, err := f.catalog.CompleteNotePublicationScan(t.Context(), store.NotePublicationCompleteRequest{
			Name: "notes", ScanID: scan.ScanID, PolicyFingerprint: scan.PolicyFingerprint, InventoryComplete: true,
		})
		require.NoError(t, err)
	}
	complete(begin()) // Absence invalidates the queued work.
	_, err = f.worker.RunOne(t.Context())
	require.NoError(t, err)
	scan := begin()
	page, err := f.catalog.NotePublicationSources(t.Context(), store.NotePublicationSourcesRequest{Name: "notes", ScanID: scan.ScanID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	_, err = f.catalog.AcknowledgePublishedNotes(t.Context(), store.NotePublicationSeenRequest{Name: "notes", ScanID: scan.ScanID, Bindings: []store.PublishedNoteIdentity{page.Items[0].Identity}})
	require.NoError(t, err)
	complete(scan)
	require.NoError(t, backfill.Run(t.Context()))
	processed, err = f.worker.RunOne(t.Context())
	require.NoError(t, err)
	require.True(t, processed)
	hits, _, err := f.catalog.SearchPage(t.Context(), "Recoveryword", 20)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, node.CurrentVersionID, hits[0].Node.CurrentVersionID)
}
