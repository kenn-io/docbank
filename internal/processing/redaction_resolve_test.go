package processing

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/production"
	"go.kenn.io/docbank/internal/redactiontest"
	"go.kenn.io/docbank/internal/store"
)

//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestRedactionResolveSelection(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t, "SECRET KEEP SECRET")
	prep, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.NoError(t, err)
	masks := []RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "text", MapSHA256: prep.Map.SHA256, Span: &redaction.Span{End: 6}}}}
	request := RedactionResolveRequest{Preparation: prep, Masks: masks}
	first, err := f.service.ResolveNativePDF(t.Context(), request, runtime)
	require.NoError(t, err)
	tag, err := f.catalog.CreateTag(t.Context(), "pending")
	require.NoError(t, err)
	_, err = f.catalog.AssignTag(t.Context(), tag.ID, prep.Selection.NodeID, store.UnconditionalRev)
	require.NoError(t, err)
	_, _, err = f.catalog.Move(t.Context(), prep.Selection.NodeID, f.catalog.RootID(), "renamed.pdf", store.UnconditionalRev)
	require.NoError(t, err)
	second, err := f.service.ResolveNativePDF(t.Context(), request, runtime)
	require.NoError(t, err)
	require.Equal(t, first.SHA256, second.SHA256)
	require.Greater(t, second.ObservedNodeRevision, first.ObservedNodeRevision)
	_, err = f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.ErrorIs(t, err, store.ErrStaleRevision)
	replacement, err := f.blobs.WriteDetailedContext(t.Context(), strings.NewReader("replacement"))
	require.NoError(t, err)
	_, _, err = f.catalog.ReplaceContent(t.Context(), prep.Selection.NodeID, store.UnconditionalRev, replacement.Hash, replacement.Size, "text/plain", processingBlobPhysical(t, replacement))
	require.NoError(t, err)
	historical, err := f.service.ResolveNativePDF(t.Context(), request, runtime)
	require.NoError(t, err)
	require.Equal(t, first.SHA256, historical.SHA256)
	_, _, err = f.catalog.Trash(t.Context(), prep.Selection.NodeID, store.UnconditionalRev)
	require.NoError(t, err)
	_, err = f.service.ResolveNativePDF(t.Context(), request, runtime)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, _, err = f.catalog.Restore(t.Context(), prep.Selection.NodeID, store.UnconditionalRev)
	require.NoError(t, err)
	_, err = f.catalog.PruneContentVersions(t.Context(), prep.Selection.NodeID, store.UnconditionalRev, store.VersionPruneSelector{VersionIDs: []string{prep.Selection.Source.VersionID}}, true)
	require.NoError(t, err)
	_, err = f.service.ResolveNativePDF(t.Context(), request, runtime)
	require.ErrorIs(t, err, store.ErrNotFound)
	entries, err := os.ReadDir(f.service.spoolDirectory)
	require.NoError(t, err)
	require.Empty(t, entries)
}

//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestRedactionResolveFreshEvidence(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t, "SECRET KEEP SECRET")
	prep, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.NoError(t, err)
	// Keep all geometry and byte lengths; reseal both observations and map.
	encoded, err := json.Marshal(prep)
	require.NoError(t, err)
	var forged RedactionPreparation
	require.NoError(t, json.Unmarshal(encoded, &forged))
	forged.Evidence.Pages[0].Glyphs[0].Text = "P"
	forged.Map.Text = "P" + forged.Map.Text[1:]
	evidenceBytes, err := canonical.Marshal(forged.Evidence)
	require.NoError(t, err)
	forged.EvidenceSHA256 = sha256HexBytes(evidenceBytes)
	forged.Map.EvidenceSHA256 = forged.EvidenceSHA256
	_, forged.Map.SHA256, err = redaction.CanonicalTextMap(forged.Map)
	require.NoError(t, err)
	require.NoError(t, production.ValidateNativePreparationDerivation(forged.NativePreparation))
	masks := []RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "page", MapSHA256: forged.Map.SHA256, Pages: []int{1}}}}
	_, err = f.service.ResolveNativePDF(t.Context(), RedactionResolveRequest{Preparation: forged, Masks: masks}, runtime)
	require.ErrorIs(t, err, ErrRedactionPreparationChanged)
}

//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestRedactionOperationAdmittedRead(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t, "SECRET KEEP SECRET")
	prep, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.NoError(t, err)
	request := RedactionResolveRequest{Preparation: prep, Masks: []RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "page", MapSHA256: prep.Map.SHA256, Pages: []int{1}}}}}
	entered, resume := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	paused, err := blob.New(&citationPausedCatalog{PackCatalog: store.NewPackCatalog(f.catalog), entered: entered, resume: resume}, filepath.Join(filepath.Dir(f.databasePath), "blobs"))
	require.NoError(t, err)
	defer func() { require.NoError(t, paused.Close()) }()
	f.service.blobs = paused
	done := make(chan error, 1)
	go func() { _, err := f.service.ResolveNativePDF(t.Context(), request, runtime); done <- err }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("not admitted: %v", err)
	}
	_, err = f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	var busy *NativePDFBusyError
	require.ErrorAs(t, err, &busy)
	require.Equal(t, "resolve", busy.Operation)
	require.NoError(t, f.service.gate.MutateContext(t.Context(), func() error {
		_, _, err := f.catalog.Trash(t.Context(), prep.Selection.NodeID, store.UnconditionalRev)
		return err
	}))
	release()
	require.NoError(t, <-done)
	_, err = f.service.ResolveNativePDF(t.Context(), request, runtime)
	require.ErrorIs(t, err, store.ErrNotFound)
	require.NoError(t, f.service.Shutdown(context.Background()))
}
