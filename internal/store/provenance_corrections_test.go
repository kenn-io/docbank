package store_test

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestProvenanceCorrectionsUpdateOnlyTheirObservedVersion(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	run, err := s.BeginCallerSuppliedIngest(t.Context(), "test", "synthetic import")
	require.NoError(t, err)
	node, err := s.IngestFileExact(t.Context(), run, s.RootID(), "report.txt",
		strings.Repeat("a", 64), 1, "text/plain", "/synthetic/report.txt", "2024-01-02T03:04:05Z")
	require.NoError(t, err)
	originalVersion := node.CurrentVersionID
	page, err := s.NodeProvenance(t.Context(), node.ID, 10, 0)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	identity := page.Items[0].Identity
	bindings, err := s.ProvenanceVersionBindings(t.Context(), originalVersion)
	require.NoError(t, err)
	require.Len(t, bindings, 1)
	plan, err := s.PreviewInitialAudit(t.Context(), s.RootID(), "cli", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(t.Context(), plan)
	require.NoError(t, err)
	require.NoError(t, s.EnsureDocumentEventRecipe(t.Context(), store.DocumentEventsDeriverFingerprint))
	before := publishCorrectionTimeline(t, s, originalVersion)

	node, _, err = s.ReplaceContent(t.Context(), node.ID, node.Revision,
		strings.Repeat("b", 64), 2, "text/plain")
	require.NoError(t, err)
	current := publishCorrectionTimeline(t, s, node.CurrentVersionID)
	for _, mtime := range []*string{new("2024-02-03T04:05:06Z"), new("2024-02-03T04:05:06Z"), nil} {
		corrected, err := s.AppendNodeProvenance(t.Context(), store.ProvenanceAppendInput{
			NodeID: node.ID, IfRevision: node.Revision, SourceKind: "test",
			SourceDescription: "synthetic correction", OriginalPath: "/synthetic/report.txt",
			OriginalMTime: mtime, Supersedes: &identity,
		})
		require.NoError(t, err)
		node, identity = corrected.Node, corrected.Fact.Identity
		_, err = s.DocumentEventsForVersion(t.Context(), originalVersion)
		require.ErrorIs(t, err, store.ErrNotFound, "a correction retires the superseded timeline head")
		targets, err := s.MissingDocumentEventTargetsAfter(t.Context(), store.DocumentEventsDeriverFingerprint, "", 10)
		require.NoError(t, err)
		require.Len(t, targets, 1, "only the exact observed version needs derivation")
		assert.Equal(t, originalVersion, targets[0].ContentVersionID)
		version, err := s.ContentVersionByID(t.Context(), originalVersion)
		require.NoError(t, err)
		evidence, err := s.LoadDocumentEventEvidence(t.Context(), store.DocumentEventTarget{
			ContentVersionID: version.ID, NodeID: version.NodeID, BlobHash: version.BlobHash,
			Size: version.Size, MIMEType: version.MimeType, RecordedAt: version.RecordedAt,
		})
		require.NoError(t, err)
		require.Len(t, evidence.Bindings, 1)
		assert.Equal(t, mtime, evidence.Bindings[0].OriginalMTime)
		assert.Equal(t, bindings[0], evidence.Bindings[0].Binding, "the original observation authority is retained")
		after := publishCorrectionTimeline(t, s, originalVersion)
		assert.NotEqual(t, before.Generation.InputsSHA256, after.Generation.InputsSHA256)
		var modified, imported []string
		for _, event := range after.Events.Events {
			if event.DateKind == "modified" {
				modified = append(modified, event.RawValue)
			}
			if event.DateKind == "imported" {
				imported = append(imported, event.RawValue)
			}
		}
		assert.Equal(t, []string{bindings[0].ObservedAt}, imported)
		if mtime == nil {
			assert.Empty(t, modified)
		} else {
			assert.Equal(t, []string{*mtime}, modified)
		}
		before = after
		unchanged, err := s.DocumentEventsForVersion(t.Context(), node.CurrentVersionID)
		require.NoError(t, err)
		assert.Equal(t, current.Generation.GenerationID, unchanged.Generation.GenerationID)
	}
	// A node-only assertion has no exact version observation to inherit.
	var unbound *string
	for range 2 {
		appended, err := s.AppendNodeProvenance(t.Context(), store.ProvenanceAppendInput{
			NodeID: node.ID, IfRevision: node.Revision, SourceKind: "test",
			SourceDescription: "synthetic assertion", OriginalPath: "/synthetic/unbound.txt",
			OriginalMTime: new("2024-03-04T05:06:07Z"), Supersedes: unbound,
		})
		require.NoError(t, err)
		node, unbound = appended.Node, &appended.Fact.Identity
		targets, err := s.MissingDocumentEventTargetsAfter(t.Context(), store.DocumentEventsDeriverFingerprint, "", 10)
		require.NoError(t, err)
		assert.Empty(t, targets)
	}

	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored, err := store.Open(filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	restoredView := publishCorrectionTimeline(t, restored, originalVersion)
	assert.Equal(t, before.Events, restoredView.Events, "restored authority derives the corrected timeline")
}

func publishCorrectionTimeline(t *testing.T, s *store.Store, versionID string) store.DocumentEventView {
	t.Helper()
	require.NoError(t, s.EnsureDocumentEventRecipe(t.Context(), store.DocumentEventsDeriverFingerprint))
	targets, err := s.MissingDocumentEventTargetsAfter(t.Context(), store.DocumentEventsDeriverFingerprint, "", 10)
	require.NoError(t, err)
	for _, target := range targets {
		if target.ContentVersionID != versionID {
			continue
		}
		evidence, err := s.LoadDocumentEventEvidence(t.Context(), target)
		require.NoError(t, err)
		record, _, err := processing.DeriveDocumentEvents(evidence)
		require.NoError(t, err)
		canonical, _, err := document.MarshalDocumentEventsV1(record)
		require.NoError(t, err)
		_, err = s.PublishDocumentEvents(t.Context(), target, store.DocumentEventsDeriverFingerprint,
			evidence.InputsSHA256, canonical)
		require.NoError(t, err)
		view, err := s.DocumentEventsForVersion(t.Context(), versionID)
		require.NoError(t, err)
		return view
	}
	require.FailNow(t, "version must be selected for timeline derivation", versionID)
	return store.DocumentEventView{}
}
