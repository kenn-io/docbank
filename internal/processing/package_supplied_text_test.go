package processing

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestPackageSuppliedTextPublishesExactVersionAndSearchableEvidence(t *testing.T) {
	// Omitting rendition publication, binding the text as the source, or
	// indexing another version must make this end-to-end assertion fail.
	f := newPublicationFixture(t)
	source, err := f.catalog.ContentVersionByID(t.Context(), f.versionID)
	require.NoError(t, err)
	textBytes := []byte("quenchwood sender text")
	textReceipt, err := f.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(textBytes))
	require.NoError(t, err)
	textNode, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), "sender.txt",
		textReceipt.Hash, textReceipt.Size, "text/plain", processingBlobPhysical(t, textReceipt))
	require.NoError(t, err)
	textVersion, err := f.catalog.ContentVersionByID(t.Context(), textNode.CurrentVersionID)
	require.NoError(t, err)

	generationID, err := PublishPackageSuppliedText(t.Context(), f.catalog, f.blobs,
		uuid.NewString(), "row-1", source, textVersion, "utf-8")
	require.NoError(t, err)
	require.NotEmpty(t, generationID)
	active, err := f.catalog.ActiveLexicalGeneration(t.Context())
	require.NoError(t, err)
	require.Equal(t, generationID, active.ID)
	hits, _, err := f.catalog.SearchPage(t.Context(), "quenchwood", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, source.ID, hits[0].Node.CurrentVersionID)
	require.Equal(t, store.SearchMatchContent, hits[0].Match)

	// The sender's text remains a separate immutable content version.
	separate, err := f.catalog.ContentVersionByID(t.Context(), textVersion.ID)
	require.NoError(t, err)
	require.Equal(t, textReceipt.Hash, separate.BlobHash)
}

func TestPackageSuppliedTextRejectsUnsupportedNativeBeforePublication(t *testing.T) {
	// Accepting a native format the provider does not declare would attach a
	// searchable rendition without a valid provider capability.
	f := newPublicationFixture(t)
	receipt, err := f.blobs.WriteDetailedContext(t.Context(), strings.NewReader("synthetic image bytes"))
	require.NoError(t, err)
	node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), "source.jpg",
		receipt.Hash, receipt.Size, "image/jpeg", processingBlobPhysical(t, receipt))
	require.NoError(t, err)
	source, err := f.catalog.ContentVersionByID(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	_, err = PublishPackageSuppliedText(t.Context(), f.catalog, f.blobs,
		uuid.NewString(), "row-2", source, source, "utf-8")
	require.Error(t, err)
	require.ErrorContains(t, err, "invalid package supplied-text authority")
	// A valid distinct text version still cannot authorize this native.
	textReceipt, err := f.blobs.WriteDetailedContext(t.Context(), strings.NewReader("unicorn text"))
	require.NoError(t, err)
	textNode, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), "sender.txt",
		textReceipt.Hash, textReceipt.Size, "text/plain", processingBlobPhysical(t, textReceipt))
	require.NoError(t, err)
	textVersion, err := f.catalog.ContentVersionByID(t.Context(), textNode.CurrentVersionID)
	require.NoError(t, err)
	_, err = PublishPackageSuppliedText(t.Context(), f.catalog, f.blobs,
		uuid.NewString(), "row-2", source, textVersion, "utf-8")
	require.ErrorContains(t, err, "unsupported native media type")
	hits, _, err := f.catalog.SearchPage(t.Context(), "unicorn", 10)
	require.NoError(t, err)
	require.Empty(t, hits)
}
