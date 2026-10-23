package processing

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/redactiontest"
)

//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestNativeRedactionRenderPhases(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t, "SECRET KEEP SECRET")
	prep, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.NoError(t, err)
	masks := []RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "text", MapSHA256: prep.Map.SHA256, Span: &redaction.Span{End: 6}}}}
	review, err := f.service.ResolveNativePDF(t.Context(), RedactionResolveRequest{Preparation: prep, Masks: masks}, runtime)
	require.NoError(t, err)
	request := RedactionRenderRequest{Preparation: prep, Masks: masks, ReviewSHA256: review.SHA256}
	for _, scenario := range []string{"success", "replaced PDF path", "changed verified PDF", "corrupt PDF", "cleanup failure", "PDF cap", "PNG cap", "ZIP cap"} {
		t.Run(scenario, func(t *testing.T) {
			controls := nativeRedactionControls{limits: defaultNativeRedactionLimits}
			verified, packaged, sent := false, false, false
			var originalPDF []byte
			controls.hooks.beforeVerify = func(file *os.File) error {
				rel, err := filepath.Rel(f.service.spoolDirectory, file.Name())
				require.NoError(t, err)
				require.True(t, filepath.IsLocal(rel))
				for _, pattern := range []string{"docbank-source-*", "docbank-fresh-*"} {
					files, err := filepath.Glob(filepath.Join(filepath.Dir(file.Name()), pattern))
					require.NoError(t, err)
					require.Empty(t, files)
				}
				if scenario == "corrupt PDF" {
					_, err := file.WriteAt([]byte("broken"), 0)
					return err
				}
				return nil
			}
			controls.hooks.afterVerify = func(dir string) error {
				verified = true
				pdfs, err := filepath.Glob(filepath.Join(dir, "redacted-*.pdf"))
				require.NoError(t, err)
				require.Len(t, pdfs, 1)
				originalPDF, err = os.ReadFile(pdfs[0])
				require.NoError(t, err)
				if scenario == "replaced PDF path" {
					require.NoError(t, os.Rename(pdfs[0], pdfs[0]+".original"))
					require.NoError(t, os.WriteFile(pdfs[0], []byte("replacement"), 0600))
				}
				if scenario == "changed verified PDF" {
					require.NoError(t, os.WriteFile(pdfs[0], bytes.Repeat([]byte("x"), len(originalPDF)), 0600))
				}
				files, err := filepath.Glob(filepath.Join(dir, "*.png"))
				require.NoError(t, err)
				require.NotEmpty(t, files)
				snapshots, err := filepath.Glob(filepath.Join(dir, "docbank-verify-*"))
				require.NoError(t, err)
				require.Empty(t, snapshots)
				if scenario == "cleanup failure" {
					require.NoError(t, os.Remove(files[0]))
					require.NoError(t, os.Mkdir(files[0], 0700))
					require.NoError(t, os.WriteFile(filepath.Join(files[0], "occupied"), []byte("synthetic"), 0600))
				}
				return nil
			}
			controls.hooks.beforePackage = func(dir string) error {
				packaged = true
				files, err := filepath.Glob(filepath.Join(dir, "*.png"))
				require.NoError(t, err)
				require.Empty(t, files)
				return nil
			}
			if scenario == "PDF cap" {
				controls.limits.pdf = 64
			}
			if scenario == "ZIP cap" {
				controls.limits.archive = 64
			}
			if scenario == "PNG cap" {
				controls.limits.png = 64
			}
			var output bytes.Buffer
			err := f.service.renderNativePDF(t.Context(), request, runtime, func(_ context.Context, r io.Reader, a RedactionArchive) error {
				sent = true
				require.Equal(t, review.SHA256, a.Receipt.ReviewSHA256)
				_, err := io.Copy(&output, r)
				return err
			}, controls)
			if scenario == "success" || scenario == "replaced PDF path" {
				require.NoError(t, err)
				require.True(t, verified)
				require.True(t, packaged)
				require.True(t, sent)
				archive, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
				require.NoError(t, err)
				require.Len(t, archive.File, 3)
				require.Equal(t, []string{"redacted.pdf", "redacted.txt", "receipt.json"}, []string{archive.File[0].Name, archive.File[1].Name, archive.File[2].Name})
				pdfStream, err := archive.File[0].Open()
				require.NoError(t, err)
				pdfBytes, err := io.ReadAll(pdfStream)
				require.NoError(t, err)
				require.NoError(t, pdfStream.Close())
				require.Equal(t, sha256HexBytes(originalPDF), sha256HexBytes(pdfBytes))
				stream, err := archive.File[1].Open()
				require.NoError(t, err)
				text, err := io.ReadAll(stream)
				require.NoError(t, err)
				require.NoError(t, stream.Close())
				require.Equal(t, "[REDACTED] KEEP SECRET\f", string(text))
			} else {
				require.Error(t, err)
				require.False(t, sent)
				require.Equal(t, scenario == "ZIP cap" || scenario == "changed verified PDF", packaged)
				if scenario == "changed verified PDF" {
					require.ErrorIs(t, err, ErrRedactionPreparationIntegrity)
				}
				if scenario == "corrupt PDF" {
					require.ErrorIs(t, err, ErrRedactionPreparationIntegrity)
					require.False(t, verified)
				}
				if scenario == "PDF cap" || scenario == "PNG cap" || scenario == "ZIP cap" {
					require.ErrorIs(t, err, ErrRedactionPreparationLimit)
				}
			}
			entries, err := os.ReadDir(f.service.spoolDirectory)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
	// Every failed operation must relinquish admission, not only delete its files.
	_, err = f.service.ResolveNativePDF(t.Context(), RedactionResolveRequest{Preparation: prep, Masks: masks}, runtime)
	require.NoError(t, err)
	// Shutdown after a completed transfer must not turn success into a transport failure.
	err = f.service.RenderNativePDF(t.Context(), request, runtime,
		func(_ context.Context, reader io.Reader, _ RedactionArchive) error {
			_, err := io.Copy(io.Discard, reader)
			require.NoError(t, err)
			f.service.Stop()
			return nil
		})
	require.NoError(t, err)
	require.NoError(t, f.service.Shutdown(t.Context()))
	entries, err := os.ReadDir(f.service.spoolDirectory)
	require.NoError(t, err)
	require.Empty(t, entries)
}

//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestRedactionOperationRenderCancellation(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t, "SECRET KEEP SECRET")
	prep, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.NoError(t, err)
	masks := []RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "page", MapSHA256: prep.Map.SHA256, Pages: []int{1}}}}
	request := RedactionResolveRequest{Preparation: prep, Masks: masks}
	review, err := f.service.ResolveNativePDF(t.Context(), request, runtime)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- f.service.RenderNativePDF(ctx, RedactionRenderRequest{Preparation: prep, Masks: masks, ReviewSHA256: review.SHA256}, runtime, func(ctx context.Context, _ io.Reader, _ RedactionArchive) error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("render stopped before delivery: %v", err)
	}
	_, err = f.service.ResolveNativePDF(t.Context(), request, runtime)
	busy, ok := errors.AsType[*NativePDFBusyError](err)
	require.True(t, ok)
	require.Equal(t, "render", busy.Operation)
	_, err = f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.ErrorIs(t, err, ErrRedactionPreparationBusy)
	require.NoError(t, f.service.gate.MaintainContext(t.Context(), func() error { return nil }), "capture must end before transmission")
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	_, err = f.service.ResolveNativePDF(t.Context(), request, runtime)
	require.NoError(t, err)
	entries, err := os.ReadDir(f.service.spoolDirectory)
	require.NoError(t, err)
	require.Empty(t, entries)
}
