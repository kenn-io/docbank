package api_test

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
)

func TestTextNavigationCaptureOrdering(t *testing.T) {
	t.Parallel()
	for _, cancelRead := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "cancel"}[cancelRead], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				gate := &pausedCitationGate{OperationGate: api.NewOperationGate(), entered: make(chan struct{}),
					resume: make(chan struct{})}
				f := newCitationHTTPFixture(t, gate)
				f.server.Close()
				purge := processing.DerivativePurgeRequest{
					AttachmentIDs: []string{f.citation.RenditionAttachmentID}}
				plan, err := f.service.PlanDerivativePurge(t.Context(), purge)
				require.NoError(t, err)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				var outline document.TextOutlinePage
				go func() {
					var err error
					outline, err = f.service.GetTextOutline(ctx,
						document.TextOutlineRequest{Citation: f.citation})
					done <- err
				}()
				<-gate.entered
				purged := make(chan error, 1)
				go func() {
					_, err := f.service.RunDerivativePurge(t.Context(), processing.DerivativePurgeJobRequest{
						DerivativePurgeRequest: purge, PlanFingerprint: plan.Fingerprint})
					purged <- err
				}()
				synctest.Wait()
				select {
				case err := <-purged:
					t.Fatalf("purge overtook capture: %v", err)
				default:
				}
				if cancelRead {
					cancel()
				} else {
					close(gate.resume)
				}
				err = <-done
				if cancelRead {
					require.ErrorIs(t, err, context.Canceled)
					require.Empty(t, outline.Sections)
					close(gate.resume)
				} else {
					require.NoError(t, err)
					require.NotEmpty(t, outline.Sections)
				}
				require.NoError(t, <-purged)
				_, err = f.service.GetTextOutline(t.Context(),
					document.TextOutlineRequest{Citation: f.citation})
				require.ErrorIs(t, err, document.ErrCitationUnavailable)
			})
		})
	}
}
