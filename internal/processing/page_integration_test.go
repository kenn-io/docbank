package processing

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image/color"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/backupapp"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/maintenance"
	"go.kenn.io/docbank/internal/store"
	"go.kenn.io/kit/backup"
)

func pageProofPDF() []byte {
	pages := []string{"/MediaBox [0 0 612 792]", "/MediaBox [0 0 595.2756 841.8898]", "/MediaBox [0 0 792 612]"}
	for _, rotation := range []int{0, 90, 180, 270} {
		pages = append(pages, fmt.Sprintf("/MediaBox [-10 -20 210 320] /CropBox [10.125 20.25 170.625 250.75] /Rotate %d", rotation))
	}
	content := "1 0 0 rg 20 30 50 70 re f\n0 0 1 rg 100 170 60 70 re f\n0 0 0 RG 12 22 155 225 re S\n"
	var children bytes.Buffer
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", ""}
	for index, page := range pages {
		fmt.Fprintf(&children, "%d 0 R ", index+3)
		objects = append(objects, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /Resources << >> /Contents %d 0 R %s >>", len(pages)+3, page))
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", children.String(), len(pages))
	objects = append(objects, fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content))
	var out bytes.Buffer
	out.WriteString("%PDF-1.7\n")
	var offsets []int
	for index, object := range objects {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return out.Bytes()
}

func pageProofPNG() []byte {
	data := mediatest.PNG(254, 508, color.White)
	chunk := make([]byte, 21)
	binary.BigEndian.PutUint32(chunk, 9)
	copy(chunk[4:], "pHYs")
	binary.BigEndian.PutUint32(chunk[8:], 10000)
	binary.BigEndian.PutUint32(chunk[12:], 10000)
	chunk[16] = 1
	binary.BigEndian.PutUint32(chunk[17:], crc32.ChecksumIEEE(chunk[4:17]))
	out := append([]byte{}, data[:33]...)
	out = append(out, chunk...)
	return append(out, data[33:]...)
}

func TestRealPageAPIBackupRestoreAndGC(t *testing.T) {
	engine := pageWorkerRuntime(t)
	root := t.TempDir()
	catalog, err := store.Open(filepath.Join(root, "docbank.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = catalog.Close() })
	blobs, err := blob.New(store.NewPackCatalog(catalog), filepath.Join(root, "blobs"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = blobs.Close() })
	cfg := config.Default()
	cfg.Server.APIKey = "synthetic-page-key"
	gate := api.NewOperationGate()
	server := api.NewServer(api.Deps{Store: catalog, Blobs: blobs, VaultRoot: root, Cfg: cfg, Gate: gate, PageRuntime: engine})
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	httpClient := client.New(httpServer.URL, cfg.Server.APIKey)
	worker, err := NewPageWorker(catalog, blobs, engine, gate)
	require.NoError(t, err)
	add := func(name, mediaType string, data []byte) store.PageBinding {
		hash, size, err := blobs.Write(bytes.NewReader(data))
		require.NoError(t, err)
		node, err := catalog.CreateFile(t.Context(), catalog.RootID(), name, hash, size, mediaType)
		require.NoError(t, err)
		return store.PageBinding{NodeID: node.ID, Revision: node.Revision, Source: document.PageSource{VersionID: node.CurrentVersionID, SHA256: hash, Size: size}}
	}
	selection := add("geometry.pdf", "application/pdf", pageProofPDF())
	run := func(selection store.PageBinding, pages []int, dpi float64) store.PageRenderJob {
		request := api.PageRenderRequest{OperationID: uuid.NewString(), Selection: selection, Pages: pages, DPI: dpi}
		job, err := httpClient.CreatePageRenderJob(t.Context(), request)
		require.NoError(t, err)
		retry, err := httpClient.CreatePageRenderJob(t.Context(), request)
		require.NoError(t, err)
		require.Equal(t, job.ID, retry.ID)
		processed, err := worker.RunOne(t.Context())
		require.NoError(t, err)
		require.True(t, processed)
		job, err = httpClient.PageRenderJob(t.Context(), job.ID, selection)
		require.NoError(t, err)
		require.Equal(t, "completed", job.State, job.FailureCode)
		return job
	}
	first := run(selection, []int{1, 5}, 144)
	require.Len(t, first.Results, 2)
	inventory, err := httpClient.PageInventory(t.Context(), selection)
	require.NoError(t, err)
	require.Equal(t, 7, inventory.Inventory.PageCount)
	require.Len(t, inventory.Inventory.Images, 2)
	// Capture actual partial authority before the rest of the document is rendered.
	repo, err := backup.Init(filepath.Join(t.TempDir(), "backup"))
	require.NoError(t, err)
	manifest, err := backupapp.Create(t.Context(), repo, "synthetic-page-proof", catalog, blobs, backup.CreateOptions{Jobs: 1})
	require.NoError(t, err)
	require.NotNil(t, manifest)
	target := filepath.Join(t.TempDir(), "restored")
	_, err = backupapp.Restore(t.Context(), repo, "synthetic-page-proof", backup.RestoreOptions{TargetDir: target, Jobs: 1})
	require.NoError(t, err)
	restored, err := store.OpenForRestore(filepath.Join(target, "docbank.db"), store.DefaultSQLiteDriver())
	require.NoError(t, err)
	t.Cleanup(func() { _ = restored.Close() })
	restoredInventory, err := restored.PageInventory(t.Context(), selection)
	require.NoError(t, err)
	require.Len(t, restoredInventory.Frames, 7)
	require.Len(t, restoredInventory.Images, 2)
	rest := run(selection, []int{2, 3, 4, 6, 7}, 144)
	require.Len(t, rest.Results, 5)
	inventory, err = httpClient.PageInventory(t.Context(), selection)
	require.NoError(t, err)
	require.Len(t, inventory.Inventory.Images, 7)
	expected := map[int][2]int64{1: {1224, 1584}, 2: {1191, 1684}, 3: {1584, 1224}, 4: {321, 461}, 5: {461, 321}, 6: {321, 461}, 7: {461, 321}}
	for _, receipt := range inventory.Inventory.Images {
		selected := api.PageImageRequest{NodeID: selection.NodeID, Revision: selection.Revision, VersionID: selection.Source.VersionID, SourceSHA256: selection.Source.SHA256, SourceSize: selection.Source.Size, Page: receipt.Page, RecipeSHA256: receipt.RecipeSHA256, FrameSHA256: receipt.FrameSHA256, ImageSHA256: receipt.SHA256}
		data, err := httpClient.ReadPageImage(t.Context(), selected)
		require.NoError(t, err)
		image, err := png.Decode(bytes.NewReader(data))
		require.NoError(t, err)
		require.Equal(t, [2]int64{int64(image.Bounds().Dx()), int64(image.Bounds().Dy())}, expected[receipt.Page])
		if receipt.Page == 5 {
			// In a 90-degree rotation source red rectangle x=20..70,y=30..100
			// maps near output (60,60); blue x=100..160,y=170..240 near (360,240).
			red, ok := color.NRGBAModel.Convert(image.At(60, 60)).(color.NRGBA)
			require.True(t, ok)
			blue, ok := color.NRGBAModel.Convert(image.At(360, 240)).(color.NRGBA)
			require.True(t, ok)
			require.Equal(t, color.NRGBA{R: 255, A: 255}, red)
			require.Equal(t, color.NRGBA{B: 255, A: 255}, blue)
			if directory := os.Getenv("DOCBANK_PAGE_PROOF_DIR"); directory != "" {
				require.True(t, filepath.IsAbs(directory))
				require.NoError(t, os.WriteFile(filepath.Join(directory, "cropped-rotated.png"), data, 0600))
			}
		}
		selected.FrameSHA256 = selection.Source.SHA256
		_, err = httpClient.ReadPageImage(t.Context(), selected)
		require.Error(t, err)
	}
	pngSelection := add("density.png", "image/png", pageProofPNG())
	native := run(pngSelection, []int{1}, 0)
	require.Equal(t, int64(254), native.Results[0].Width)
	report, err := maintenance.GarbageCollect(t.Context(), catalog, blobs, maintenance.GCOptions{Budget: maintenance.Budget{MaxObjects: 1000}})
	require.NoError(t, err)
	require.Zero(t, report.RemovedBlobs)
	require.NoError(t, catalog.VerifyPageImageBytes(t.Context(), blobs))
}
