package processing

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/redactiontest"
	"go.kenn.io/docbank/internal/store"
)

type preparationFixture struct {
	publicationFixture
	service *Service
	request RedactionPreparationRequest
}

func newPreparationFixture(t *testing.T) preparationFixture {
	t.Helper()
	f := newPublicationFixture(t)
	data := redactiontest.PDF(t, []string{"SECRET KEEP SECRET"}, "")
	receipt, err := f.blobs.WriteDetailedContext(t.Context(), bytes.NewReader(data))
	require.NoError(t, err)
	node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), "native.pdf",
		receipt.Hash, receipt.Size, "application/pdf", processingBlobPhysical(t, receipt))
	require.NoError(t, err)
	service, err := NewService(ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newTestOperationGate(),
		SpoolDirectory: filepath.Join(t.TempDir(), "spool")})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Shutdown(context.Background())) })
	return preparationFixture{publicationFixture: f, service: service, request: RedactionPreparationRequest{
		Selection: store.PageBinding{NodeID: node.ID, Revision: node.Revision,
			Source: document.PageSource{VersionID: node.CurrentVersionID, SHA256: receipt.Hash, Size: receipt.Size}}}}
}

func TestRedactionPreparationSelection(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t)
	result, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.NoError(t, err)
	require.Equal(t, "SECRET KEEP SECRET", result.Map.Text)
	require.Equal(t, f.request.Selection, result.Selection)
	for _, test := range []struct {
		name   string
		change func(*RedactionPreparationRequest)
		want   error
	}{
		{"missing version", func(r *RedactionPreparationRequest) {
			r.Selection.Source.VersionID = "00000000-0000-4000-8000-000000000001"
		}, store.ErrNotFound},
		{"wrong owner", func(r *RedactionPreparationRequest) { r.Selection.NodeID = f.catalog.RootID() }, store.ErrNotFound},
		{"stale before hash", func(r *RedactionPreparationRequest) {
			r.Selection.Revision++
			r.Selection.Source.SHA256 = strings.Repeat("a", 64)
		}, store.ErrStaleRevision},
		{"bad hash", func(r *RedactionPreparationRequest) { r.Selection.Source.SHA256 = strings.Repeat("a", 64) }, ErrRedactionPreparationInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := f.request
			test.change(&request)
			_, err := f.service.PrepareNativePDF(t.Context(), request, runtime)
			require.ErrorIs(t, err, test.want)
		})
	}
	receipt, err := f.blobs.WriteDetailedContext(t.Context(), strings.NewReader("replacement"))
	require.NoError(t, err)
	node, _, err := f.catalog.ReplaceContent(t.Context(), f.request.Selection.NodeID, store.UnconditionalRev,
		receipt.Hash, receipt.Size, "text/plain", processingBlobPhysical(t, receipt))
	require.NoError(t, err)
	f.request.Selection.Revision = node.Revision
	old, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.NoError(t, err)
	require.Equal(t, result.Map, old.Map)
	_, _, err = f.catalog.Trash(t.Context(), node.ID, store.UnconditionalRev)
	require.NoError(t, err)
	_, err = f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestRedactionPreparationUnavailable(t *testing.T) {
	f := newPreparationFixture(t)
	_, err := f.service.PrepareNativePDF(t.Context(), f.request, nil)
	require.ErrorIs(t, err, ErrRedactionPreparationUnavailable)
	f.request.Selection.Source.Size = document.MaxPageSourceBytes + 1
	_, err = f.service.PrepareNativePDF(t.Context(), f.request, nil)
	require.ErrorIs(t, err, ErrRedactionPreparationLimit)
}

func TestRedactionPreparationAdmittedRead(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t)
	entered, resume := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	paused, err := blob.New(&citationPausedCatalog{PackCatalog: store.NewPackCatalog(f.catalog),
		entered: entered, resume: resume}, filepath.Join(filepath.Dir(f.databasePath), "blobs"))
	require.NoError(t, err)
	defer func() { require.NoError(t, paused.Close()) }()
	f.service.blobs = paused
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	var result RedactionPreparation
	go func() { var err error; result, err = f.service.PrepareNativePDF(ctx, f.request, runtime); done <- err }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("capture stopped before I/O: %v", err)
	}
	_, err = f.service.PrepareNativePDF(ctx, f.request, runtime)
	require.ErrorIs(t, err, ErrRedactionPreparationBusy)
	err = f.service.gate.MutateContext(ctx, func() error {
		_, _, err := f.catalog.Trash(ctx, f.request.Selection.NodeID, store.UnconditionalRev)
		return err
	})
	require.NoError(t, err)
	release()
	require.NoError(t, <-done)
	require.Equal(t, "SECRET KEEP SECRET", result.Map.Text)
	require.Equal(t, f.request.Selection, result.Selection)
}

func TestRedactionPreparationShutdown(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t)
	entered, resume := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	paused, err := blob.New(&citationPausedCatalog{PackCatalog: store.NewPackCatalog(f.catalog),
		entered: entered, resume: resume}, filepath.Join(filepath.Dir(f.databasePath), "blobs"))
	require.NoError(t, err)
	defer func() { require.NoError(t, paused.Close()) }()
	f.service.blobs = paused
	done := make(chan error, 1)
	go func() { _, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime); done <- err }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("not admitted: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, f.service.Shutdown(ctx))
	require.ErrorIs(t, <-done, ErrRedactionPreparationUnavailable)
	_, err = f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.ErrorIs(t, err, ErrRedactionPreparationUnavailable)
	require.False(t, errors.Is(err, context.Canceled))
}

func TestRedactionPreparationResultLimit(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t)
	result, err := f.service.prepareNativePDF(t.Context(), f.request, runtime, 1)
	require.ErrorIs(t, err, ErrRedactionPreparationLimit)
	require.Empty(t, result.Map.Text)
}

func TestRedactionPreparationCanceledReadReleasesSlot(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t)
	entered, resume := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	paused, err := blob.New(&citationPausedCatalog{PackCatalog: store.NewPackCatalog(f.catalog),
		entered: entered, resume: resume}, filepath.Join(filepath.Dir(f.databasePath), "blobs"))
	require.NoError(t, err)
	defer func() { require.NoError(t, paused.Close()) }()
	f.service.blobs = paused
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.service.PrepareNativePDF(ctx, f.request, runtime); done <- err }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("not admitted: %v", err)
	}
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	release()
	result, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.NoError(t, err)
	require.Equal(t, "SECRET KEEP SECRET", result.Map.Text)
}

func TestRedactionPreparationPageLimit(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t)
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.SetFont("Helvetica", "", 12)
	for range 17 {
		pdf.AddPage()
		pdf.Text(72, 72, "retained native text")
	}
	var data bytes.Buffer
	require.NoError(t, pdf.Output(&data))
	receipt, err := f.blobs.WriteDetailedContext(t.Context(), &data)
	require.NoError(t, err)
	node, version, err := f.catalog.ReplaceContent(t.Context(), f.request.Selection.NodeID,
		store.UnconditionalRev, receipt.Hash, receipt.Size, "application/pdf", processingBlobPhysical(t, receipt))
	require.NoError(t, err)
	f.request.Selection = store.PageBinding{NodeID: node.ID, Revision: node.Revision,
		Source: document.PageSource{VersionID: version.ID, SHA256: receipt.Hash, Size: receipt.Size}}
	result, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.ErrorIs(t, err, ErrRedactionPreparationLimit)
	require.Empty(t, result.Map.Text)
}
