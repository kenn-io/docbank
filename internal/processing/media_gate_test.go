package processing

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMediaMutationsWaitForMaintenance(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"submit", "declare", "revoke"} {
		t.Run(operation, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			gate := &observedMutationGate{workerTestGate: newWorkerTestGate(), arrived: make(chan struct{}, 1)}
			service, err := NewService(t.Context(), ServiceConfig{
				Catalog: fixture.catalog, Blobs: fixture.blobs, Gate: gate,
				SpoolDirectory: t.TempDir(), Principal: "daemon:operator", MediaTokenKey: [32]byte{1},
				MediaOrigins: map[string]MediaOriginPolicy{"synthetic": {
					OriginID: "synthetic", Provider: "synthetic",
					ResolverFingerprint: processingHash("resolver"), IdentityFingerprint: processingHash("identity"),
					DisclosureFingerprint: processingHash("disclosure"), InputClasses: []string{"recording_reference"},
					RetainedClasses: []string{"recording_bytes"}, ReferencePrefixes: []string{"https://recordings.invalid/"},
				}},
			})
			require.NoError(t, err)
			retained, err := service.SubmitRemoteRecording(t.Context(), RemoteRecordingRequest{
				OperationID: "00000000-0000-4000-8000-000000000501", ReferenceURL: "https://recordings.invalid/call",
				Occurrence: MediaOccurrenceInput{Ref: "call", Revision: "1"},
			})
			require.NoError(t, err)
			mutate := func(ctx context.Context) (MediaReceipt, error) {
				const operationID = "00000000-0000-4000-8000-000000000502"
				switch operation {
				case "submit":
					return service.SubmitRemoteRecording(ctx, RemoteRecordingRequest{
						OperationID: operationID, ReferenceURL: "https://recordings.invalid/another-call",
						Occurrence: MediaOccurrenceInput{Ref: "another-call", Revision: "1"},
					})
				case "declare":
					return service.DeclareMediaOccurrence(ctx, operationID, retained.SourceID,
						MediaOccurrenceInput{Ref: "copy", Revision: "1"})
				default:
					return service.RevokeMediaOccurrence(ctx, operationID, retained.OccurrenceID, "1")
				}
			}
			var before bytes.Buffer
			require.NoError(t, fixture.catalog.ExportMetadata(t.Context(), &before))
			require.NoError(t, gate.MaintainContext(t.Context(), func() error {
				ctx, cancel := context.WithCancel(context.WithValue(t.Context(), observedMutation{}, true))
				defer cancel()
				mutated := make(chan error, 1)
				go func() {
					_, err := mutate(ctx)
					mutated <- err
				}()
				select {
				case <-gate.arrived:
				case err := <-mutated:
					require.FailNow(t, "the mutation finished without the operation gate", "error: %v", err)
				}
				// Maintenance holds the gate, so the mutation can only end by cancellation.
				cancel()
				require.ErrorIs(t, <-mutated, context.Canceled)
				var after bytes.Buffer
				require.NoError(t, fixture.catalog.ExportMetadata(t.Context(), &after))
				require.Equal(t, before.String(), after.String())
				return nil
			}))
			receipt, err := mutate(t.Context())
			require.NoError(t, err)
			require.Equal(t, "succeeded", receipt.OperationState)
		})
	}
}

type observedMutation struct{}

// observedMutationGate reports when a mutation marked with observedMutation
// reaches the operation gate.
type observedMutationGate struct {
	*workerTestGate

	arrived chan struct{}
}

func (gate *observedMutationGate) MutateContext(ctx context.Context, fn func() error) error {
	gate.observe(ctx)
	return gate.workerTestGate.MutateContext(ctx, fn)
}

func (gate *observedMutationGate) CaptureContext(ctx context.Context, fn func() error) error {
	gate.observe(ctx)
	return gate.workerTestGate.CaptureContext(ctx, fn)
}

func (gate *observedMutationGate) observe(ctx context.Context) {
	if ctx.Value(observedMutation{}) != nil {
		select {
		case gate.arrived <- struct{}{}:
		default:
		}
	}
}
