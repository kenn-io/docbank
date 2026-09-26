package store_test

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/store"
)

// The screenshot harness copies this synthetic vault after all writers close.
// It then opens the copy through the real daemon and browser.
func TestProductionEditorScreenshotFixture(t *testing.T) {
	ready := os.Getenv("DOCBANK_PRODUCTION_EDITOR_FIXTURE_READY")
	done := os.Getenv("DOCBANK_PRODUCTION_EDITOR_FIXTURE_DONE")
	if ready == "" || done == "" {
		t.Skip("opt-in production editor screenshot fixture")
	}
	vault, root, _, _, member, pdf := store.ProductionPreviewStageFixture(t)
	createUndecided := func(name string, createID, addID string) redaction.Set {
		t.Helper()
		set, draft, err := vault.CreateProductionSet(t.Context(), "synthetic-operator", redaction.CreateRequest{
			OperationID: createID, Name: name})
		require.NoError(t, err)
		_, err = vault.ApplyProductionChanges(t.Context(), "synthetic-operator", set.ID, draft.Revision,
			redaction.ApplyRequest{OperationID: addID, ETag: draft.ETag,
				Changes: []redaction.Change{{Kind: "member", Member: &member}}})
		require.NoError(t, err)
		return set
	}
	set := createUndecided("Synthetic editor", "89000000-0000-4000-8000-000000000051",
		"89000000-0000-4000-8000-000000000052")
	_ = createUndecided("Synthetic keep", "89000000-0000-4000-8000-000000000053",
		"89000000-0000-4000-8000-000000000054")
	blobs, err := blob.New(store.NewPackCatalog(vault), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	written, err := blobs.WriteDetailedContext(t.Context(), bytes.NewReader(pdf))
	require.NoError(t, err)
	require.Equal(t, member.PDFSHA256, written.Hash)
	require.NoError(t, vault.RecordBlob(t.Context(), written.Hash, written.Size, store.BlobPhysical{
		Encoding: "raw", StoredBytes: written.StoredSize, Created: written.Created,
	}))
	require.NoError(t, blobs.Close())
	require.NoError(t, vault.Close())
	data, err := json.Marshal(struct {
		Root  string `json:"root"`
		SetID string `json:"set_id"`
	}{Root: root, SetID: set.ID})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(ready, data, 0o600))
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(done); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("screenshot harness did not finish copying the synthetic vault")
}
