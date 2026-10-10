package api_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
)

type pausedCitationGate struct {
	*api.OperationGate

	entered, resume chan struct{}
	once            sync.Once
}

func (g *pausedCitationGate) CaptureContext(ctx context.Context, fn func() error) error {
	return g.OperationGate.CaptureContext(ctx, func() error {
		g.once.Do(func() { close(g.entered) })
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-g.resume:
			return fn()
		}
	})
}

func TestTextCitationCaptureOrdering(t *testing.T) {
	t.Parallel()
	for _, cancelRead := range []bool{false, true} {
		name := map[bool]string{false: "read then purge", true: "cancel then purge"}[cancelRead]
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				gate := &pausedCitationGate{OperationGate: api.NewOperationGate(), entered: make(chan struct{}),
					resume: make(chan struct{})}
				f := newCitationHTTPFixture(t, gate)
				f.server.Close()
				request := processing.DerivativePurgeRequest{
					AttachmentIDs: []string{f.citation.RenditionAttachmentID},
				}
				plan, err := f.service.PlanDerivativePurge(t.Context(), request)
				require.NoError(t, err)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				readDone := make(chan error, 1)
				var quote document.ResolvedTextCitation
				go func() {
					var err error
					quote, err = f.service.ResolveTextCitation(ctx, f.citation)
					readDone <- err
				}()
				<-gate.entered
				purgeDone := make(chan error, 1)
				var receipt processing.DerivativePurgeReceipt
				go func() {
					var err error
					receipt, err = f.service.RunDerivativePurge(t.Context(),
						processing.DerivativePurgeJobRequest{
							DerivativePurgeRequest: request, PlanFingerprint: plan.Fingerprint,
						})
					purgeDone <- err
				}()
				synctest.Wait()
				select {
				case err := <-purgeDone:
					t.Fatalf("purge overtook capture: %v", err)
				default:
				}
				if cancelRead {
					cancel()
				} else {
					close(gate.resume)
				}
				err = <-readDone
				if cancelRead {
					require.ErrorIs(t, err, context.Canceled)
					require.Empty(t, quote.Text)
				} else {
					require.NoError(t, err)
					require.Equal(t, "Verified alpha", quote.Text)
				}
				require.NoError(t, <-purgeDone)
				require.Equal(t, 1, receipt.RemovedAttachments)
				if cancelRead {
					close(gate.resume)
				}
				_, err = f.service.ResolveTextCitation(t.Context(), f.citation)
				require.ErrorIs(t, err, document.ErrCitationUnavailable)
			})
		})
	}
}

func TestTextCitationHTTPDeadline(t *testing.T) {
	t.Parallel()
	for _, cancelRead := range []bool{false, true} {
		name := map[bool]string{false: "daemon timeout", true: "caller cancellation"}[cancelRead]
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				gate := api.NewOperationGate()
				f := newCitationHTTPFixture(t, gate)
				f.server.Close()
				entered, resume := make(chan struct{}), make(chan struct{})
				release := sync.OnceFunc(func() { close(resume) })
				defer release()
				maintenanceDone := make(chan error, 1)
				go func() {
					maintenanceDone <- gate.MaintainContext(t.Context(), func() error {
						close(entered)
						<-resume
						return nil
					})
				}()
				<-entered
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				encoded, err := json.Marshal(f.citation)
				require.NoError(t, err)
				request := httptest.NewRequestWithContext(ctx, http.MethodPost,
					citationPath, bytes.NewReader(encoded))
				request.Header.Set("X-Api-Key", testAPIKey)
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				done := make(chan struct{})
				go func() { f.store.Server.Handler().ServeHTTP(response, request); close(done) }()
				synctest.Wait()
				if cancelRead {
					cancel()
				} else {
					time.Sleep(61 * time.Second)
				}
				<-done
				wantStatus, wantCode := 504, "citation_timeout"
				if cancelRead {
					wantStatus, wantCode = 408, "citation_canceled"
				}
				require.Equal(t, wantStatus, response.Code, response.Body.String())
				require.Contains(t, response.Body.String(), `"code":"`+wantCode+`"`)
				require.NotContains(t, response.Body.String(), `"text":`)
				release()
				require.NoError(t, <-maintenanceDone)
				got, err := f.service.ResolveTextCitation(t.Context(), f.citation)
				require.NoError(t, err)
				require.Equal(t, "Verified alpha", got.Text)
			})
		})
	}
}

func TestTextCitationCaptureDuringBackupFreeze(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		gate := api.NewOperationGate()
		f := newCitationHTTPFixture(t, gate)
		f.server.Close()
		end := api.BeginTextCitationTestFreeze(t, gate)
		defer end()
		got, err := f.service.ResolveTextCitation(t.Context(), f.citation)
		require.NoError(t, err)
		require.Equal(t, "Verified alpha", got.Text)
	})
}
