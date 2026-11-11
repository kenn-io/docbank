package processing

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/pagerender"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/redactiontest"
	"go.kenn.io/docbank/internal/store"
)

func recipientBuildFixture(t *testing.T) (preparationFixture, RedactionRenderRequest, *pagerender.Runtime) {
	t.Helper()
	ctx := t.Context()
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t, "SECRET KEEP SECRET")
	prep, err := f.service.PrepareNativePDF(ctx, f.request, runtime)
	require.NoError(t, err)
	masks := []RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{
		Kind: "text", MapSHA256: prep.Map.SHA256, Span: &redaction.Span{End: 6},
	}}}
	review, err := BuildRedactionReview(prep, masks, 1, pdfproduction.QualifiedRecipe())
	require.NoError(t, err)
	ns, err := f.catalog.EnsureBatesNamespace(ctx, "HANDOFF", "", 6)
	require.NoError(t, err)
	r, err := f.service.ReserveNumberedPDF(ctx, NumberedReservationRequest{
		Preparation: prep, Masks: masks, ReviewSHA256: review.SHA256,
		NamespaceID: uuid.MustParse(ns.NamespaceID), StartAt: 1, RetainOriginal: true,
	}, runtime)
	require.NoError(t, err)
	request := RedactionRenderRequest{Preparation: prep, Masks: masks,
		ReviewSHA256: review.SHA256, AllocationID: r.AllocationID}
	return f, request, runtime
}

//nolint:paralleltest // Qualified PDFium measures whole-process RSS.
func TestBuildRecipientPackageChecksRetainedPDF(t *testing.T) {
	f, request, _ := recipientBuildFixture(t)
	ctx := t.Context()
	allocation, pages, err := f.catalog.BatesPublicationPlan(ctx, request.AllocationID.String())
	require.NoError(t, err)
	ns, err := f.catalog.BatesNamespace(ctx, allocation.NamespaceID, "", "", 0)
	require.NoError(t, err)
	review, err := MatchRedactionReview(request.Preparation, request.Masks, request.Preparation.Selection.Revision, request.ReviewSHA256)
	require.NoError(t, err)
	recipe, err := BuildNumberedRecipe(uuid.MustParse(f.catalog.VaultID()), ns, 1, review)
	require.NoError(t, err)
	encoded, err := canonical.Marshal(recipe)
	require.NoError(t, err)
	// The recorded bytes and manifest agree, but the PDF has no redaction or footer.
	// Publication storage trusts its caller's verification; packaging must check again.
	source, _, err := f.blobs.OpenStreamContext(ctx, request.Preparation.Selection.Source.SHA256)
	require.NoError(t, err)
	written, err := f.blobs.WriteDetailedContext(ctx, source)
	require.NoError(t, err)
	require.NoError(t, source.Close())
	_, err = f.catalog.PublishBatesArtifact(ctx, store.BatesArtifactPublication{
		ArtifactID: allocation.AllocationID, AllocationID: allocation.AllocationID,
		BlobSHA256: written.Hash, Size: written.Size, PageCount: 1, RecipeJSON: encoded, Pages: pages,
	}, processingBlobPhysical(t, written))
	require.NoError(t, err)
	_, err = f.service.BuildRecipientPackage(ctx, "alice", request, nil)
	require.ErrorIs(t, err, pdfproduction.ErrPDFIntegrity)
	files, err := filepath.Glob(filepath.Join(f.service.spoolDirectory, "native-pdf-*"))
	require.NoError(t, err)
	require.Empty(t, files, "failed verification must not leave a downloadable package")
}

//nolint:paralleltest // Qualified PDFium measures whole-process RSS.
func TestBuildRecipientPackage(t *testing.T) {
	ctx := t.Context()
	f, request, runtime := recipientBuildFixture(t)
	var err error
	_, err = f.service.BuildRecipientPackage(ctx, "alice", request, nil)
	require.ErrorIs(t, err, ErrRecipientPackageNotPublished)
	artifact, err := f.service.PublishNumberedPDF(ctx, request, runtime)
	require.NoError(t, err)
	_, _, err = f.catalog.Trash(ctx, f.request.Selection.NodeID, store.UnconditionalRev)
	require.NoError(t, err)
	_, err = f.catalog.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)

	// The completed-artifact path has no runtime parameter or live-node admission.
	result, err := f.service.BuildRecipientPackage(ctx, "alice", request, nil)
	require.NoError(t, err)
	_, err = f.service.BuildRecipientPackage(ctx, "alice", request, nil)
	require.ErrorIs(t, err, ErrRecipientPackageBusy)
	op, err := f.service.beginNativePDFOperation(ctx, "another operation")
	require.NoError(t, err, "ready package must release the native slot")
	require.NoError(t, op.Close())
	_, err = f.service.AcquireRecipientPackage(ctx, "bob", result.ID)
	require.ErrorIs(t, err, ErrRecipientPackageUnavailable)
	require.NoError(t, f.service.DiscardRecipientPackages(t.Context(), "bob"))
	reader, err := f.service.AcquireRecipientPackage(ctx, "alice", result.ID)
	require.NoError(t, err)
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, result.Size, int64(len(data)))
	require.Equal(t, result.SHA256, fmt.Sprintf("%x", sha256.Sum256(data)))
	z, err := zip.NewReader(reader, result.Size)
	require.NoError(t, err)
	require.Len(t, z.File, 6)
	entries := map[string][]byte{}
	for _, entry := range z.File {
		source, err := entry.Open()
		require.NoError(t, err)
		entries[entry.Name], err = io.ReadAll(source)
		require.NoError(t, err)
		require.NoError(t, source.Close())
	}
	require.Equal(t, artifact.BlobSHA256, fmt.Sprintf("%x", sha256.Sum256(entries["VOL001/PDF/000001.pdf"])))
	require.Equal(t, "[REDACTED] KEEP SECRET\f", string(entries["VOL001/TEXT/000001.txt"]))
	require.Equal(t, "HANDOFF000001,VOL001,PDF\\000001.pdf,Y,,,1\r\n", string(entries["VOL001/DATA.OPT"]))
	files, err := filepath.Glob(filepath.Join(f.service.spoolDirectory, "native-pdf-*", "*"))
	require.NoError(t, err)
	require.Len(t, files, 1, "ready entry retains only its ZIP")
	released := make(chan error, 1)
	go func() { released <- f.service.DiscardRecipientPackages(t.Context(), "alice") }()
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case <-reader.Context().Done():
	case <-deadline.Done():
		t.Fatal("owner release did not cancel the active delivery")
	}
	require.ErrorIs(t, reader.Context().Err(), context.Canceled)
	select {
	case err := <-released:
		t.Fatalf("release returned before the active delivery closed: %v", err)
	default:
	}
	require.NoError(t, reader.Close())
	require.NoError(t, <-released)
	_, err = os.Stat(filepath.Dir(files[0]))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = f.service.AcquireRecipientPackage(ctx, "alice", result.ID)
	require.ErrorIs(t, err, ErrRecipientPackageUnavailable)
}

//nolint:paralleltest // Qualified PDFium measures whole-process RSS.
func TestBuildRecipientPackageCanceledPublication(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	f, request, runtime := recipientBuildFixture(t)
	_, err := f.service.PublishNumberedPDF(t.Context(), request, runtime)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var directory string
	t.Cleanup(func() {
		if directory != "" {
			require.NoError(t, os.Chmod(directory, 0o700))
		}
		require.NoError(t, f.service.DiscardRecipientPackages(t.Context(), "alice"))
	})
	_, err = f.service.BuildRecipientPackage(ctx, "alice", request, func(commit func() error) error {
		dirs, err := filepath.Glob(filepath.Join(f.service.spoolDirectory, "native-pdf-*"))
		require.NoError(t, err)
		require.Len(t, dirs, 1)
		directory = dirs[0]
		require.NoError(t, os.Chmod(directory, 0o500))
		probe, err := os.Create(filepath.Join(directory, "permission-probe"))
		if err == nil {
			require.NoError(t, probe.Close())
			require.NoError(t, os.Chmod(directory, 0o700))
			t.Skip("platform does not enforce Unix directory permissions")
		}
		cancel()
		return commit()
	})
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, fs.ErrPermission)
	require.ErrorIs(t, f.service.DiscardRecipientPackages(t.Context(), "alice"), fs.ErrPermission)
	op, err := f.service.beginNativePDFOperation(t.Context(), "another operation")
	require.NoError(t, err, "failed cleanup retains package capacity, not the native slot")
	require.NoError(t, op.Close())
	shutdown, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	require.NoError(t, f.service.Shutdown(shutdown))
	require.NoError(t, os.Chmod(directory, 0o700))
	require.NoError(t, f.service.DiscardRecipientPackages(t.Context(), "alice"))
	directory = ""
}
