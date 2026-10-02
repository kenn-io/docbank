package processing

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/store"
)

func TestMediaStatusSeparatesLatestAttemptFromSuccessfulCoverage(t *testing.T) {
	t.Parallel()
	f := newMediaStateFixture(t)
	fixture, service := f.publicationFixture, f.service
	retained, err := service.SubmitSuppliedMedia(t.Context(), f.suppliedRequest(uuid.New().String(), nil))
	require.NoError(t, err)
	status, err := service.MediaStatus(t.Context(), retained.SourceID)
	require.NoError(t, err)
	require.Equal(t, retained.OperationID, status.OperationID)
	require.Equal(t, "succeeded", status.OperationState)
	secondOccurrence, err := service.DeclareMediaOccurrence(t.Context(), uuid.New().String(),
		retained.SourceID, MediaOccurrenceInput{Ref: "second", Revision: "1"})
	require.NoError(t, err)
	importInput := func(occurrenceID, text string) string {
		t.Helper()
		input, err := service.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
			OperationID: uuid.New().String(), SourceID: retained.SourceID, OccurrenceID: occurrenceID,
			Kind: "transcript", Filename: "call.txt", MediaType: "text/plain",
			SHA256: processingHash(text), ByteLength: int64(len(text)), Content: strings.NewReader(text),
		})
		require.NoError(t, err)
		return input.SuppliedInputID
	}
	firstInput := importInput(retained.OccurrenceID, "first synthetic transcript")
	secondInput := importInput(secondOccurrence.OccurrenceID, "second synthetic transcript")
	queue := func(operationID, inputID, jobID string) store.MediaPublicationReceipt {
		t.Helper()
		op := store.MediaOperation{ID: operationID, Principal: service.principal,
			Verb: "retry_media", SourceID: retained.SourceID, RequestSHA256: processingHash(inputID)}
		receipt := store.MediaPublicationReceipt{VaultUID: retained.VaultUID, SourceID: retained.SourceID,
			SourceVersionID: retained.SourceVersionID, ContentVersionID: retained.ContentVersionID,
			OccurrenceID: retained.OccurrenceID, OperationID: op.ID, OperationState: "queued",
			CoverageState: "pending", ProcessingProfile: "speech", SuppliedInputID: inputID,
			ProcessingProfileFingerprint: service.profiles["speech"].record.Fingerprint}
		receipt, err := fixture.catalog.QueueMediaRetry(t.Context(), op, receipt)
		require.NoError(t, err)
		if jobID != "" {
			receipt, err = fixture.catalog.SetMediaProcessingJob(t.Context(), op.ID, service.principal, jobID)
			require.NoError(t, err)
		}
		return receipt
	}
	check := func(t *testing.T, attempt store.MediaPublicationReceipt, state, coverage string) {
		t.Helper()
		projection, err := fixture.catalog.MediaSource(t.Context(), service.principal, retained.SourceID)
		require.NoError(t, err)
		require.NotEmpty(t, projection.ProcessingReceipts)
		assert.Equal(t, attempt.OperationID, projection.ProcessingReceipts[0].OperationID)
		status, err := service.MediaStatus(t.Context(), retained.SourceID)
		require.NoError(t, err)
		assert.Equal(t, attempt.OperationID, status.OperationID)
		assert.Equal(t, state, status.OperationState)
		assert.Equal(t, attempt.JobID, status.JobID)
		assert.Equal(t, attempt.SuppliedInputID, status.SuppliedInputID)
		assert.Equal(t, coverage, status.CoverageState)
		page, err := service.ListMediaSources(t.Context(), MediaListOptions{Limit: 10})
		require.NoError(t, err)
		require.Len(t, page.Items, 1)
		assert.Equal(t, coverage, page.Items[0].CoverageState)
	}
	// Match the operation-ID tie-break when the clock gives attempts equal timestamps.
	job := f.enqueue(t, f.selector)
	first := queue("00000000-0000-4000-8000-000000000201", firstInput, job.ID)
	t.Run("first queued attempt", func(t *testing.T) { check(t, first, "queued", "pending") })
	_, err = fixture.catalog.FailMediaProcessing(t.Context(), first.OperationID, service.principal)
	require.NoError(t, err)
	t.Run("first failed attempt", func(t *testing.T) { check(t, first, "failed", "unavailable") })
	first = queue("00000000-0000-4000-8000-000000000202", firstInput, job.ID)
	require.NoError(t, f.run(t, job.ID))
	t.Run("rendition completed", func(t *testing.T) { check(t, first, "succeeded", "transcribed") })
	second := queue("00000000-0000-4000-8000-000000000203", secondInput, "")
	t.Run("retry after success", func(t *testing.T) { check(t, second, "queued", "transcribed") })
	_, err = fixture.catalog.FailMediaProcessing(t.Context(), second.OperationID, service.principal)
	require.NoError(t, err)
	t.Run("failed retry", func(t *testing.T) { check(t, second, "failed", "transcribed") })
	_, err = service.RevokeMediaOccurrence(t.Context(), uuid.New().String(), retained.OccurrenceID, "1")
	require.NoError(t, err)
	t.Run("successful input revoked", func(t *testing.T) { check(t, second, "failed", "stale") })
}

func TestMediaCursorRejectsAnotherPrincipal(t *testing.T) {
	t.Parallel()
	fixture := newPublicationFixture(t)
	var key [32]byte
	key[0] = 4
	newService := func(principal string) *Service {
		service, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
			Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Principal: principal,
			MediaTokenKey: key})
		require.NoError(t, err)
		return service
	}
	owner := newService("operator:owner")
	token, err := owner.signMediaPage(mediaPageClaim{Version: "v2", Principal: "operator:owner",
		Kind: "sources", After: "source", Limit: 1})
	require.NoError(t, err)
	_, err = newService("operator:other").ListMediaSources(t.Context(), MediaListOptions{
		Cursor: token, Limit: 1})
	require.ErrorIs(t, err, ErrMediaCursorInvalid)
}

func TestMediaRevocationAcrossOperations(t *testing.T) {
	t.Parallel()
	fixture := newPublicationFixture(t)
	config := ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Principal: "operator:owner",
		MediaTokenKey: [32]byte{1}, MediaOrigins: map[string]MediaOriginPolicy{"synthetic": {
			OriginID: "synthetic", Provider: "synthetic",
			ReferencePrefixes: []string{"https://recordings.invalid/"},
		}}}
	owner, err := NewService(config)
	require.NoError(t, err)
	config.Principal = "operator:other"
	other, err := NewService(config)
	require.NoError(t, err)
	retained, err := owner.SubmitRemoteRecording(t.Context(), RemoteRecordingRequest{
		OperationID: uuid.New().String(), ReferenceURL: "https://recordings.invalid/call",
		Occurrence: MediaOccurrenceInput{Ref: "call", Revision: "1"},
	})
	require.NoError(t, err)

	var first MediaReceipt
	for _, phase := range []string{"visible", "revoked"} {
		for _, check := range []struct {
			name, revision string
			service        *Service
			wantErr        error
		}{
			{"wrong revision", "2", owner, store.ErrMediaOccurrenceConflict},
			{"another principal", "1", other, store.ErrNotFound},
			{"another principal wrong revision", "2", other, store.ErrNotFound},
		} {
			t.Run(phase+"/"+check.name, func(t *testing.T) {
				_, err := check.service.RevokeMediaOccurrence(t.Context(), uuid.New().String(),
					retained.OccurrenceID, check.revision)
				require.ErrorIs(t, err, check.wantErr)
			})
		}
		operationID := uuid.New().String()
		receipt, err := owner.RevokeMediaOccurrence(t.Context(), operationID, retained.OccurrenceID, "1")
		require.NoError(t, err)
		require.Equal(t, "succeeded", receipt.OperationState)
		require.Equal(t, retained.SourceID, receipt.SourceID)
		require.Equal(t, retained.OccurrenceID, receipt.OccurrenceID)
		require.Equal(t, operationID, receipt.OperationID)
		if phase == "visible" {
			first = receipt
		} else {
			first.OperationID = operationID
			require.Equal(t, first, receipt)
		}
		replayed, err := owner.RevokeMediaOccurrence(t.Context(), operationID, retained.OccurrenceID, "1")
		require.NoError(t, err)
		require.Equal(t, receipt, replayed)
	}
}

// TestMediaListPagesByIDAcrossChanges catches a list cursor that breaks, or
// repeats rows, when occurrences are declared or revoked between pages.
func TestMediaListPagesByIDAcrossChanges(t *testing.T) {
	t.Parallel()
	fixture := newPublicationFixture(t)
	owner, err := NewService(ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
		Gate: newWorkerTestGate(), SpoolDirectory: t.TempDir(), Principal: "operator:pages",
		MediaTokenKey: [32]byte{2}, MediaOrigins: map[string]MediaOriginPolicy{"synthetic": {
			OriginID: "synthetic", Provider: "synthetic", ReferencePrefixes: []string{"https://recordings.invalid/"},
		}}})
	require.NoError(t, err)
	retained, err := owner.SubmitRemoteRecording(t.Context(), RemoteRecordingRequest{
		OperationID: uuid.New().String(), ReferenceURL: "https://recordings.invalid/pages",
		Occurrence: MediaOccurrenceInput{Ref: "page-0", Revision: "1"},
	})
	require.NoError(t, err)
	visible := map[string]bool{retained.OccurrenceID: true}
	declare := func(ref string) string {
		receipt, err := owner.DeclareMediaOccurrence(t.Context(), uuid.New().String(), retained.SourceID,
			MediaOccurrenceInput{Ref: ref, Revision: "1"})
		require.NoError(t, err)
		return receipt.OccurrenceID
	}
	for _, ref := range []string{"page-1", "page-2"} {
		visible[declare(ref)] = true
	}

	page, err := owner.ListMediaOccurrences(t.Context(), MediaListOptions{Limit: 1})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	returned := page.Items[0].OccurrenceID
	_, err = owner.RevokeMediaOccurrence(t.Context(), uuid.New().String(), returned, "1")
	require.NoError(t, err)
	for index := 0; ; index++ {
		ref := fmt.Sprintf("late-%d", index)
		if mediaOccurrenceID(owner.principal, ref, "1") < returned {
			declare(ref)
			break
		}
	}
	tampered := "A" + page.NextCursor
	_, err = owner.ListMediaOccurrences(t.Context(), MediaListOptions{Cursor: tampered, Limit: 1})
	require.ErrorIs(t, err, ErrMediaCursorInvalid)

	seen := []string{returned}
	for page.NextCursor != "" {
		page, err = owner.ListMediaOccurrences(t.Context(), MediaListOptions{Cursor: page.NextCursor, Limit: 1})
		require.NoError(t, err)
		for _, item := range page.Items {
			seen = append(seen, item.OccurrenceID)
		}
	}
	delete(visible, returned)
	require.Subset(t, seen, slices.Collect(maps.Keys(visible)), "each row visible throughout appears")
	require.Len(t, seen, len(visible)+1, "no row appears twice")
}
