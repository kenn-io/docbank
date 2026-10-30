package ingest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoXMPPlacementCleanupRetryExcludesPublication(t *testing.T) {
	t.Parallel()
	fixture := newTestIngester(t)
	catalog := fixture.Store
	hash, size, err := fixture.Blobs.Write(strings.NewReader("synthetic JPEG"))
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "capture.jpg", hash, size, "image/jpeg")
	require.NoError(t, err)
	asset, err := catalog.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	raw := asset.Files[0]
	_, err = catalog.EditPhotoAuthoredBatch(t.Context(), store.PhotoAuthoredBatch{Targets: []store.PhotoAuthoredEditTarget{{AssetID: asset.ID, AssetRevision: asset.Revision, FileID: raw.ID, Revision: raw.Revision, NodeRevision: node.Revision}}, Patch: store.PhotoAuthoredPatch{Rating: new(3)}})
	require.NoError(t, err)
	canonical, _, err := document.MarshalSourceMetadataV1(document.SourceMetadataV1{ContractVersion: document.SourceMetadataContractV1, Fields: []document.SourceMetadataFieldV1{}})
	require.NoError(t, err)
	_, err = catalog.PublishSourceMetadata(t.Context(), hash, strings.Repeat("a", 64), canonical)
	require.NoError(t, err)
	work, run, err := catalog.ClassifyPhotoXMP(t.Context(), raw.ID)
	require.NoError(t, err)
	require.True(t, run)
	data := []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#"><rdf:Description xmlns:xmp="http://ns.adobe.com/xap/1.0/" xmp:Rating="3"/></rdf:RDF></x:xmpmeta>`)
	plan, _ := store.PlanPhotoXMP(work, work.Steps(), "")
	plan.Packet, plan.Write = store.PhotoXMPPacket{PhotoAuthored: work.Original.Authored(), Present: store.PhotoConfirmedRating | store.PhotoXMPKeywordsBit, Keywords: work.Keywords}, true
	ing := &Ingester{Store: catalog, Blobs: fixture.Blobs}
	// Cleanup retries hold maintenance while retiring old blob paths.
	// Publishing a sidecar must wait until that maintenance finishes.
	require.NoError(t, fixture.Blobs.WithMaintenance(t.Context(), func() error {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond) //nolint:kennlint // deadlinetest: the deadline proves publication waits for active cleanup
		defer cancel()
		_, publishErr := ing.PublishPhotoXMP(ctx, plan, data)
		require.ErrorIs(t, publishErr, context.DeadlineExceeded)
		return nil
	}))
	_, err = ing.PublishPhotoXMP(t.Context(), plan, data)
	require.NoError(t, err)
}
