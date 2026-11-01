package processing

import (
	"context"
	"io"
	"os"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/redactiontest"
)

//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestRedactionPreviewVerifiedLifetime(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t, "SECRET KEEP SECRET")
	prep, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.NoError(t, err)
	masks := []RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "text", MapSHA256: prep.Map.SHA256, Span: &redaction.Span{End: 6}}}}
	review, err := f.service.ResolveNativePDF(t.Context(), RedactionResolveRequest{Preparation: prep, Masks: masks}, runtime)
	require.NoError(t, err)
	request := RedactionRenderRequest{Preparation: prep, Masks: masks, ReviewSHA256: review.SHA256}
	createCtx, cancel := context.WithCancel(t.Context())
	preview, err := f.service.CreateRedactionPreview(createCtx, "alice", request, runtime, func(publish func() error) error { return publish() })
	require.NoError(t, err)
	cancel() // Ready output must outlive its successful HTTP request.
	require.Len(t, preview.Pages, 1)
	require.Equal(t, 0, preview.Pages[0].TextStart)
	require.Equal(t, len("[REDACTED] KEEP SECRET"), preview.Pages[0].TextEnd)
	_, err = f.service.AcquireRedactionPreview(t.Context(), "bob", preview.ID, "bundle", 0)
	require.ErrorIs(t, err, ErrRedactionPreviewUnavailable)
	require.ErrorIs(t, f.service.DiscardRedactionPreview("bob", preview.ID), ErrRedactionPreviewUnavailable)
	_, err = f.service.CreateRedactionPreview(t.Context(), "bob", request, runtime, nil)
	require.ErrorIs(t, err, ErrRedactionPreviewBusy)
	// A retained preview holds no native-operation slot.
	_, err = f.service.ResolveNativePDF(t.Context(), RedactionResolveRequest{Preparation: prep, Masks: masks}, runtime)
	require.NoError(t, err)
	reader, err := f.service.AcquireRedactionPreview(t.Context(), "alice", preview.ID, "text", 0)
	require.NoError(t, err)
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, "[REDACTED] KEEP SECRET\f", string(data))
	require.NoError(t, reader.Close())
	reader, err = f.service.AcquireRedactionPreview(t.Context(), "alice", preview.ID, "bundle", 0)
	require.NoError(t, err)
	files, err := os.ReadDir(f.service.spoolDirectory)
	require.NoError(t, err)
	require.Len(t, files, 1)
	retained, err := os.ReadDir(f.service.spoolDirectory + "/" + files[0].Name())
	require.NoError(t, err)
	require.Len(t, retained, 2, "only the PNG and ZIP remain")
	require.NoError(t, f.service.DiscardRedactionPreview("alice", preview.ID))
	_, err = f.service.AcquireRedactionPreview(t.Context(), "alice", preview.ID, "bundle", 0)
	require.ErrorIs(t, err, ErrRedactionPreviewUnavailable)
	_, err = f.service.CreateRedactionPreview(t.Context(), "bob", request, runtime, nil)
	require.ErrorIs(t, err, ErrRedactionPreviewBusy)
	data, err = io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, preview.Archive.Size, int64(len(data)))
	require.Equal(t, preview.Archive.SHA256, sha256HexBytes(data))
	require.NoError(t, reader.Close())
	files, err = os.ReadDir(f.service.spoolDirectory)
	require.NoError(t, err)
	require.Empty(t, files)
}

//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestRedactionPreviewExpiryAndRevocation(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t, "SECRET")
	prep, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.NoError(t, err)
	masks := []RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "page", MapSHA256: prep.Map.SHA256, Pages: []int{1}}}}
	review, err := f.service.ResolveNativePDF(t.Context(), RedactionResolveRequest{Preparation: prep, Masks: masks}, runtime)
	require.NoError(t, err)
	req := RedactionRenderRequest{Preparation: prep, Masks: masks, ReviewSHA256: review.SHA256}
	preview, err := f.service.CreateRedactionPreview(t.Context(), "alice", req, runtime, nil)
	require.NoError(t, err)
	reader, err := f.service.AcquireRedactionPreview(t.Context(), "alice", preview.ID, "bundle", 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()
	f.service.RevokeRedactionPreviews("alice")
	select {
	case <-reader.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("revocation did not interrupt the reader")
	}
	require.ErrorIs(t, reader.Context().Err(), context.Canceled)
	require.NoError(t, reader.Close())
	preview, err = f.service.CreateRedactionPreview(t.Context(), "alice", req, runtime, nil)
	require.NoError(t, err)
	f.service.clock = func() time.Time { return preview.ExpiresAt.Add(time.Second) }
	_, err = f.service.RedactionPreview("alice", preview.ID)
	require.ErrorIs(t, err, ErrRedactionPreviewUnavailable)
	files, err := os.ReadDir(f.service.spoolDirectory)
	require.NoError(t, err)
	require.Empty(t, files)
}

//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestRedactionPreviewNeverPublishesUnverifiedOrRevokedOutput(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t, "SECRET")
	prep, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.NoError(t, err)
	masks := []RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "page", MapSHA256: prep.Map.SHA256, Pages: []int{1}}}}
	review, err := f.service.ResolveNativePDF(t.Context(), RedactionResolveRequest{Preparation: prep, Masks: masks}, runtime)
	require.NoError(t, err)
	request := RedactionRenderRequest{Preparation: prep, Masks: masks, ReviewSHA256: review.SHA256}
	for _, scenario := range []string{"corruption", "revocation during render", "revocation at publication"} {
		t.Run(scenario, func(t *testing.T) {
			controls := nativeRedactionControls{limits: defaultNativeRedactionLimits, retainPages: true}
			published := false
			controls.hooks.beforeVerify = func(file *os.File) error {
				if scenario == "corruption" {
					_, err := file.WriteAt([]byte("broken"), 0)
					return err
				}
				if scenario == "revocation during render" {
					f.service.RevokeRedactionPreviews("alice")
				}
				return nil
			}
			_, err := f.service.createRedactionPreview(t.Context(), "alice", request, runtime, func(publish func() error) error {
				if scenario == "revocation at publication" {
					f.service.RevokeRedactionPreviews("alice")
				}
				err := publish()
				published = err == nil
				return err
			}, controls)
			require.Error(t, err)
			require.False(t, published)
			entries, err := os.ReadDir(f.service.spoolDirectory)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}
