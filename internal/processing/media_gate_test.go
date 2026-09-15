package processing

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMediaMutationsWaitForMaintenance(t *testing.T) {
	for _, operation := range []string{"submit", "declare", "revoke"} {
		t.Run(operation, func(t *testing.T) {
			fixture := newPublicationFixture(t)
			gate := newWorkerTestGate()
			service, err := NewService(ServiceConfig{
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
				ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				defer cancel()
				_, err := mutate(ctx)
				require.ErrorIs(t, err, context.DeadlineExceeded)
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
