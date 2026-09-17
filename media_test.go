package docbank

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/store"
)

func TestMediaFailedPublicationLeavesNoCoreContent(t *testing.T) {
	for _, test := range []struct {
		name     string
		conflict bool
	}{{"invalid timestamp", false}, {"conflicting occurrence", true}} {
		t.Run(test.name, func(t *testing.T) {
			vault, err := New(t.Context(), Config{Root: t.TempDir()})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })
			occurrence := MediaOccurrenceInput{Ref: "call", Revision: "1"}
			if test.conflict {
				raw := mediatest.MP3()
				identity := contentIdentity(raw)
				_, err := vault.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
					OperationID: uuid.New().String(), Content: bytes.NewReader(raw),
					Filename: "call.mp3", MediaType: "audio/mpeg", SHA256: identity.SHA256,
					ByteLength: identity.Size, Occurrence: occurrence,
				})
				require.NoError(t, err)
			} else {
				// Each field fits the input limit, but JSON escaping puts the
				// occurrence's combined timestamp claim over the catalog limit.
				text := strings.Repeat("\x00", 4096)
				occurrence.Message = MediaTimestamp{Raw: text, Normalized: text, ZoneText: text}
			}
			var before bytes.Buffer
			require.NoError(t, vault.metadata.ExportMetadata(t.Context(), &before))
			raw := mediatest.WAV()
			identity := contentIdentity(raw)
			_, err = vault.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
				OperationID: uuid.New().String(), Content: bytes.NewReader(raw),
				Filename: "call.wav", MediaType: "audio/wav", SHA256: identity.SHA256,
				ByteLength: identity.Size, Occurrence: occurrence,
			})
			if test.conflict {
				require.ErrorIs(t, err, store.ErrMediaOccurrenceConflict)
			} else {
				require.ErrorIs(t, err, store.ErrMediaSourceConflict)
			}
			_, err = vault.Stat(t.Context(), "/media/"+identity.SHA256[:2]+"/"+identity.SHA256+".wav")
			require.ErrorIs(t, err, ErrNotFound, "a rejected upload must not leave a live core node")
			present, err := vault.metadata.HasBlob(t.Context(), identity.SHA256)
			require.NoError(t, err)
			require.False(t, present, "a rejected upload must not catalog its bytes")
			var after bytes.Buffer
			require.NoError(t, vault.metadata.ExportMetadata(t.Context(), &after))
			require.Equal(t, before.String(), after.String(), "publication failure must roll back all authority")
		})
	}
}

// TestMediaSuppliedOccurrencesReuseOneSealedVersion catches duplicate media
// nodes or source revisions being created for independent occurrence claims.
func TestMediaSuppliedOccurrencesReuseOneSealedVersion(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	raw := mediatest.WAV()
	identity := contentIdentity(raw)
	var first MediaReceipt
	for index, operationID := range []string{
		"00000000-0000-4000-8000-000000000001",
		"00000000-0000-4000-8000-000000000002",
		"00000000-0000-4000-8000-000000000003",
	} {
		receipt, submitErr := vault.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
			OperationID: operationID,
			Content:     bytes.NewReader(raw),
			Filename:    "synthetic.wav",
			MediaType:   "audio/wav",
			SHA256:      identity.SHA256,
			ByteLength:  identity.Size,
			Occurrence: MediaOccurrenceInput{
				Ref: "message-" + string(rune('a'+index)), Revision: "1", Filename: "synthetic.wav",
			},
		})
		require.NoError(t, submitErr)
		require.Equal(t, "succeeded", receipt.OperationState)
		require.Equal(t, "unprocessed", receipt.CoverageState)
		require.Empty(t, receipt.JobID)
		if index == 0 {
			first = receipt
		} else {
			require.Equal(t, first.SourceID, receipt.SourceID)
			require.Equal(t, first.SourceVersionID, receipt.SourceVersionID)
			require.Equal(t, first.ContentVersionID, receipt.ContentVersionID)
			require.NotEqual(t, first.OccurrenceID, receipt.OccurrenceID)
		}
	}

	node, err := vault.Stat(t.Context(), "/media/"+identity.SHA256[:2]+"/"+identity.SHA256+".wav")
	require.NoError(t, err)
	require.Equal(t, first.ContentVersionID, node.CurrentVersionID)
	versions, err := vault.Versions(t.Context(), node.ID, VersionsOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, versions.Total)
}

// TestMediaSuppliedTranscriptConsumer proves both admitted M1 codecs can use
// an occurrence-bound supplied transcript through the public embedded API.
func TestMediaSuppliedTranscriptConsumer(t *testing.T) {
	for _, test := range []struct {
		name, filename, mediaType, extension string
		raw                                  []byte
	}{
		{"wav", "call.wav", "audio/wav", ".wav", mediatest.WAV()},
		{"mp3", "call.mp3", "audio/mpeg", ".mp3", mediatest.MP3()},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			vault, err := New(t.Context(), Config{Root: root})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, vault.Close()) })
			identity := contentIdentity(test.raw)
			receipt, err := vault.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
				OperationID: "00000000-0000-4000-8000-000000000401", Content: bytes.NewReader(test.raw),
				Filename: test.filename, MediaType: test.mediaType, SHA256: identity.SHA256,
				ByteLength: identity.Size, Occurrence: MediaOccurrenceInput{Ref: "call", Revision: "1", Filename: test.filename},
			})
			require.NoError(t, err)
			phrase := fmt.Sprintf("exact supplied %s transcript phrase", test.name)
			transcript := []byte(phrase + "\n")
			transcriptIdentity := contentIdentity(transcript)
			artifact, err := vault.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
				OperationID: "00000000-0000-4000-8000-000000000402", SourceID: receipt.SourceID,
				OccurrenceID: receipt.OccurrenceID, Kind: "transcript", Origin: "supplied",
				Filename: "transcript.txt", MediaType: "text/plain", SHA256: transcriptIdentity.SHA256,
				ByteLength: transcriptIdentity.Size, Content: bytes.NewReader(transcript),
			})
			require.NoError(t, err)
			require.NotEmpty(t, artifact.SuppliedInputID)
			node, err := vault.Stat(t.Context(), "/media/"+identity.SHA256[:2]+"/"+identity.SHA256+test.extension)
			require.NoError(t, err)
			selector := ProcessingSelector{NodeID: node.ID, ContentVersionID: receipt.ContentVersionID,
				Profile: "supplied-transcript"}
			plan, err := vault.PlanProcessing(t.Context(), ProcessingPlanRequest{Selector: selector})
			require.NoError(t, err)
			_, err = vault.GrantProcessingPlanConsent(t.Context(), ProcessingConsentGrantRequest{
				PlanRequest: ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint})
			require.NoError(t, err)
			vault.processingCancel()
			vault.processingWG.Wait()
			queued, err := vault.RetryMedia(t.Context(),
				"00000000-0000-4000-8000-000000000403", receipt.SourceID,
				MediaProcessingRequest{Profile: "supplied-transcript", SuppliedInputID: artifact.SuppliedInputID})
			require.NoError(t, err)
			require.Equal(t, "queued", queued.OperationState)
			require.NoError(t, vault.Close())
			vault, err = New(t.Context(), Config{Root: root})
			require.NoError(t, err)
			var lastStatus MediaReceipt
			var lastStatusErr error
			deadline := time.Now().Add(30 * time.Second)
			for time.Now().Before(deadline) {
				lastStatus, lastStatusErr = vault.MediaStatus(t.Context(), receipt.SourceID)
				if lastStatusErr == nil && lastStatus.OperationID == queued.OperationID &&
					lastStatus.OperationState == "succeeded" && lastStatus.CoverageState == "transcribed" {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			require.NoError(t, lastStatusErr)
			require.Equal(t, queued.OperationID, lastStatus.OperationID)
			require.Equal(t, "succeeded", lastStatus.OperationState)
			require.Equal(t, "transcribed", lastStatus.CoverageState, "%+v", lastStatus)
			rendition, err := vault.Rendition(t.Context(), RenditionRequest{Selector: selector})
			require.NoError(t, err)
			raw, err := io.ReadAll(rendition.Reader)
			require.NoError(t, err)
			require.NoError(t, rendition.Reader.Verify())
			require.NoError(t, rendition.Reader.Close())
			require.Contains(t, string(raw), phrase)
			results, err := vault.SearchDocuments(t.Context(), DocumentSearchRequest{Query: phrase,
				Mode: DocumentSearchLexical, Profile: "supplied-transcript", Limit: 10,
				Fence: DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{receipt.ContentVersionID}}})
			require.NoError(t, err)
			require.NotEmpty(t, results.Results)
			require.Equal(t, receipt.ContentVersionID, results.Results[0].ContentVersionID)
		})
	}
}

func TestRemoteRecordingManualEmbedded(t *testing.T) {
	root := t.TempDir()
	vault, err := New(t.Context(), Config{Root: root})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	remote, err := vault.SubmitRemoteRecording(t.Context(), RemoteRecordingRequest{
		OperationID:  "00000000-0000-4000-8000-000000000441",
		ReferenceURL: "https://private.invalid/share/embedded?token=synthetic-secret",
		CanonicalURL: "HTTPS://Recordings.INVALID:443/share/embedded#fragment",
		Occurrence:   MediaOccurrenceInput{Ref: "embedded-call", Revision: "1", Filename: "embedded.wav"},
	})
	require.NoError(t, err)
	require.Equal(t, "unsupported", remote.Outcome)
	require.Empty(t, remote.ContentVersionID)

	raw := mediatest.WAV()
	identity := contentIdentity(raw)
	original, err := vault.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: "00000000-0000-4000-8000-000000000442", SourceID: remote.SourceID,
		OccurrenceID: remote.OccurrenceID, Kind: "media", Origin: "supplied",
		Filename: "embedded.wav", MediaType: "audio/wav", SHA256: identity.SHA256,
		ByteLength: identity.Size, Content: bytes.NewReader(raw),
	})
	require.NoError(t, err)
	require.Equal(t, "content_available", original.Outcome)

	transcript := []byte("embedded remote recording transcript phrase\n")
	transcriptIdentity := contentIdentity(transcript)
	input, err := vault.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: "00000000-0000-4000-8000-000000000443", SourceID: remote.SourceID,
		OccurrenceID: remote.OccurrenceID, Kind: "transcript", Origin: "supplied",
		Filename: "embedded.txt", MediaType: "text/plain", SHA256: transcriptIdentity.SHA256,
		ByteLength: transcriptIdentity.Size, Content: bytes.NewReader(transcript),
	})
	require.NoError(t, err)

	node, err := vault.Stat(t.Context(), "/media/"+remote.SourceID+"/"+identity.SHA256+".wav")
	require.NoError(t, err)
	selector := ProcessingSelector{NodeID: node.ID, ContentVersionID: original.ContentVersionID,
		Profile: "supplied-transcript"}
	plan, err := vault.PlanProcessing(t.Context(), ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)
	_, err = vault.GrantProcessingPlanConsent(t.Context(), ProcessingConsentGrantRequest{
		PlanRequest: ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	vault.processingCancel()
	vault.processingWG.Wait()
	queued, err := vault.RetryMedia(t.Context(), "00000000-0000-4000-8000-000000000444",
		remote.SourceID, MediaProcessingRequest{Profile: "supplied-transcript", SuppliedInputID: input.SuppliedInputID})
	require.NoError(t, err)
	require.Equal(t, "queued", queued.OperationState)
	require.NoError(t, vault.Close())
	vault, err = New(t.Context(), Config{Root: root})
	require.NoError(t, err)

	var status MediaReceipt
	require.Eventually(t, func() bool {
		status, err = vault.MediaStatus(t.Context(), remote.SourceID)
		return err == nil && status.OperationID == queued.OperationID &&
			status.OperationState == "succeeded" && status.CoverageState == "transcribed"
	}, 30*time.Second, 20*time.Millisecond)
	require.Equal(t, remote.SourceID, status.SourceID)
	require.Equal(t, original.SourceVersionID, status.SourceVersionID)
	require.Equal(t, original.ContentVersionID, status.ContentVersionID)
	results, err := vault.SearchDocuments(t.Context(), DocumentSearchRequest{
		Query: "embedded remote recording transcript phrase", Mode: DocumentSearchLexical,
		Profile: "supplied-transcript", Limit: 10,
		Fence: DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{original.ContentVersionID}},
	})
	require.NoError(t, err)
	require.NotEmpty(t, results.Results)
	require.Equal(t, original.ContentVersionID, results.Results[0].ContentVersionID)
}

func TestRemoteRecordingEqualBytesKeepOrdinaryProcessingSource(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })

	first, err := vault.SubmitRemoteRecording(t.Context(), RemoteRecordingRequest{
		OperationID:  "00000000-0000-4000-8000-000000000451",
		ReferenceURL: "https://private.invalid/a",
		CanonicalURL: "https://recordings.invalid/a",
		Occurrence:   MediaOccurrenceInput{Ref: "equal-a", Revision: "1", Filename: "a.wav"},
	})
	require.NoError(t, err)
	second, err := vault.SubmitRemoteRecording(t.Context(), RemoteRecordingRequest{
		OperationID:  "00000000-0000-4000-8000-000000000452",
		ReferenceURL: "https://private.invalid/b",
		CanonicalURL: "https://recordings.invalid/b",
		Occurrence:   MediaOccurrenceInput{Ref: "equal-b", Revision: "1", Filename: "b.wav"},
	})
	require.NoError(t, err)

	raw := mediatest.WAV()
	identity := contentIdentity(raw)
	firstMedia, err := vault.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: "00000000-0000-4000-8000-000000000453", SourceID: first.SourceID,
		OccurrenceID: first.OccurrenceID, Kind: "media", Origin: "supplied",
		Filename: "a.wav", MediaType: "audio/wav", SHA256: identity.SHA256,
		ByteLength: identity.Size, Content: bytes.NewReader(raw),
	})
	require.NoError(t, err)
	secondMedia, err := vault.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: "00000000-0000-4000-8000-000000000454", SourceID: second.SourceID,
		OccurrenceID: second.OccurrenceID, Kind: "media", Origin: "supplied",
		Filename: "b.wav", MediaType: "audio/wav", SHA256: identity.SHA256,
		ByteLength: identity.Size, Content: bytes.NewReader(raw),
	})
	require.NoError(t, err)
	require.NotEqual(t, firstMedia.ContentVersionID, secondMedia.ContentVersionID)

	transcript := []byte("second source only transcript\n")
	transcriptIdentity := contentIdentity(transcript)
	_, err = vault.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: "00000000-0000-4000-8000-000000000455", SourceID: second.SourceID,
		OccurrenceID: second.OccurrenceID, Kind: "transcript", Origin: "supplied",
		Filename: "b.txt", MediaType: "text/plain", SHA256: transcriptIdentity.SHA256,
		ByteLength: transcriptIdentity.Size, Content: bytes.NewReader(transcript),
	})
	require.NoError(t, err)
	_, err = vault.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
		OperationID: "00000000-0000-4000-8000-000000000456", Content: bytes.NewReader(raw),
		Filename: "supplied.wav", MediaType: "audio/wav", SHA256: identity.SHA256,
		ByteLength: identity.Size, Processing: &MediaProcessingRequest{Profile: "supplied-transcript"},
		Occurrence: MediaOccurrenceInput{Ref: "equal-supplied", Revision: "1", Filename: "supplied.wav"},
	})
	require.ErrorIs(t, err, store.ErrNotFound)

	node, err := vault.Stat(t.Context(), "/media/"+first.SourceID+"/"+identity.SHA256+".wav")
	require.NoError(t, err)
	selector := ProcessingSelector{NodeID: node.ID, ContentVersionID: firstMedia.ContentVersionID,
		Profile: "supplied-transcript"}
	plan, err := vault.PlanProcessing(t.Context(), ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)
	_, err = vault.StartProcessing(t.Context(), StartProcessingRequest{
		PlanRequest: ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint, Consent: true,
	})
	require.ErrorIs(t, err, store.ErrNotFound)
}

// TestMediaExistingVersionBindingKeepsOriginalPath catches media retention
// cloning a package-owned version into the managed /media namespace.
func TestMediaExistingVersionBindingKeepsOriginalPath(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	raw := mediatest.MP3()
	identity := contentIdentity(raw)
	existing, err := vault.Create(t.Context(), "/received/native.mp3", bytes.NewReader(raw), CreateOptions{
		MediaType: "audio/mpeg", Expected: identity,
	})
	require.NoError(t, err)

	receipt, err := vault.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
		OperationID: "00000000-0000-4000-8000-000000000004",
		Filename:    "native.mp3", MediaType: "audio/mpeg", SHA256: identity.SHA256,
		ByteLength: identity.Size, ExistingContentVersionID: existing.Version.ID,
		Occurrence: MediaOccurrenceInput{Ref: "package-row-1", Revision: "1", Filename: "native.mp3"},
	})
	require.NoError(t, err)
	require.Equal(t, existing.Version.ID, receipt.ContentVersionID)
	reused, err := vault.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
		OperationID: "00000000-0000-4000-8000-000000000005", Content: bytes.NewReader(raw),
		Filename: "native.mp3", MediaType: "audio/mpeg", SHA256: identity.SHA256,
		ByteLength: identity.Size,
		Occurrence: MediaOccurrenceInput{Ref: "package-row-2", Revision: "1", Filename: "native.mp3"},
	})
	require.NoError(t, err)
	require.Equal(t, existing.Version.ID, reused.ContentVersionID)
	kept, err := vault.Stat(t.Context(), "/received/native.mp3")
	require.NoError(t, err)
	require.Equal(t, existing.Node.ID, kept.ID)
	_, err = vault.Stat(t.Context(), "/media/"+identity.SHA256[:2]+"/"+identity.SHA256+".mp3")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestMediaProcessingFreezesSelectedInputAndRevocation(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	raw := mediatest.WAV()
	identity := contentIdentity(raw)
	mediaReceipt, err := vault.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
		OperationID: "00000000-0000-4000-8000-000000000421", Content: bytes.NewReader(raw),
		Filename: "selection.wav", MediaType: "audio/wav", SHA256: identity.SHA256,
		ByteLength: identity.Size,
		Occurrence: MediaOccurrenceInput{Ref: "selection-a", Revision: "1", Filename: "selection.wav"},
	})
	require.NoError(t, err)
	secondOccurrence, err := vault.DeclareMediaOccurrence(t.Context(),
		"00000000-0000-4000-8000-000000000422", mediaReceipt.SourceID,
		MediaOccurrenceInput{Ref: "selection-b", Revision: "1", Filename: "selection.wav"})
	require.NoError(t, err)
	importTranscript := func(operationID, occurrenceID, phrase string) MediaReceipt {
		t.Helper()
		content := []byte(phrase + "\n")
		contentIdentity := contentIdentity(content)
		receipt, importErr := vault.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
			OperationID: operationID, SourceID: mediaReceipt.SourceID, OccurrenceID: occurrenceID,
			Kind: "transcript", Filename: "selection.txt", MediaType: "text/plain",
			SHA256: contentIdentity.SHA256, ByteLength: contentIdentity.Size, Content: bytes.NewReader(content),
		})
		require.NoError(t, importErr)
		return receipt
	}
	older := importTranscript("00000000-0000-4000-8000-000000000423",
		mediaReceipt.OccurrenceID, "older exact transcript")

	node, err := vault.Stat(t.Context(), "/media/"+identity.SHA256[:2]+"/"+identity.SHA256+".wav")
	require.NoError(t, err)
	selector := ProcessingSelector{NodeID: node.ID, ContentVersionID: mediaReceipt.ContentVersionID,
		Profile: "supplied-transcript"}
	plan, err := vault.PlanProcessing(t.Context(), ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)
	_, err = vault.GrantProcessingPlanConsent(t.Context(), ProcessingConsentGrantRequest{
		PlanRequest: ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint})
	require.NoError(t, err)
	process := func(operationID, inputID, phrase string) MediaReceipt {
		t.Helper()
		queued, retryErr := vault.RetryMedia(t.Context(), operationID, mediaReceipt.SourceID,
			MediaProcessingRequest{Profile: "supplied-transcript", SuppliedInputID: inputID})
		require.NoError(t, retryErr)
		require.Eventually(t, func() bool {
			status, statusErr := vault.ProcessingStatus(t.Context(), ProcessingStatusRequest{JobID: queued.JobID})
			return statusErr == nil && status.State == "completed"
		}, 30*time.Second, 20*time.Millisecond)
		require.Eventually(t, func() bool {
			status, statusErr := vault.MediaStatus(t.Context(), mediaReceipt.SourceID)
			return statusErr == nil && status.OperationID == queued.OperationID &&
				status.OperationState == "succeeded" && status.CoverageState == "transcribed" &&
				status.SuppliedInputID == inputID
		}, 30*time.Second, 20*time.Millisecond)
		rendition, renditionErr := vault.Rendition(t.Context(), RenditionRequest{Selector: selector})
		require.NoError(t, renditionErr)
		body, readErr := io.ReadAll(rendition.Reader)
		require.NoError(t, readErr)
		require.NoError(t, rendition.Reader.Verify())
		require.NoError(t, rendition.Reader.Close())
		require.Contains(t, string(body), phrase)
		return queued
	}
	firstJob := process("00000000-0000-4000-8000-000000000425",
		older.SuppliedInputID, "older exact transcript")
	newer := importTranscript("00000000-0000-4000-8000-000000000424",
		secondOccurrence.OccurrenceID, "newer replacement transcript")
	require.NotEqual(t, older.SuppliedInputID, newer.SuppliedInputID)
	statusAfterImport, err := vault.MediaStatus(t.Context(), mediaReceipt.SourceID)
	require.NoError(t, err)
	require.Equal(t, "transcribed", statusAfterImport.CoverageState)
	secondJob := process("00000000-0000-4000-8000-000000000426",
		newer.SuppliedInputID, "newer replacement transcript")
	require.NotEqual(t, firstJob.JobID, secondJob.JobID)

	revoked, err := vault.RevokeMediaOccurrence(t.Context(),
		"00000000-0000-4000-8000-000000000427", secondOccurrence.OccurrenceID, "1")
	require.NoError(t, err)
	replayedRevocation, err := vault.RevokeMediaOccurrence(t.Context(),
		"00000000-0000-4000-8000-000000000427", secondOccurrence.OccurrenceID, "1")
	require.NoError(t, err)
	require.Equal(t, revoked, replayedRevocation)
	_, err = vault.Rendition(t.Context(), RenditionRequest{Selector: selector})
	require.ErrorIs(t, err, ErrNotFound)
	status, err := vault.MediaStatus(t.Context(), mediaReceipt.SourceID)
	require.NoError(t, err)
	require.Equal(t, "stale", status.CoverageState)
	results, err := vault.SearchDocuments(t.Context(), DocumentSearchRequest{Query: "newer replacement transcript",
		Mode: DocumentSearchLexical, Profile: "supplied-transcript", Limit: 10,
		Fence: DocumentSourceFence{VaultUID: vault.ID(), ContentVersionIDs: []string{mediaReceipt.ContentVersionID}}})
	require.NoError(t, err)
	require.Empty(t, results.Results)
}

func TestStartProcessingFreezesSuppliedInputBeforeRevocation(t *testing.T) {
	vault, err := New(t.Context(), Config{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, vault.Close()) })
	raw := mediatest.WAV()
	identity := contentIdentity(raw)
	mediaReceipt, err := vault.SubmitSuppliedMedia(t.Context(), SuppliedMediaRequest{
		OperationID: "00000000-0000-4000-8000-000000000431", Content: bytes.NewReader(raw),
		Filename: "ordinary.wav", MediaType: "audio/wav", SHA256: identity.SHA256,
		ByteLength: identity.Size,
		Occurrence: MediaOccurrenceInput{Ref: "ordinary-a", Revision: "1", Filename: "ordinary.wav"},
	})
	require.NoError(t, err)
	_, err = vault.DeclareMediaOccurrence(t.Context(),
		"00000000-0000-4000-8000-000000000432", mediaReceipt.SourceID,
		MediaOccurrenceInput{Ref: "ordinary-b", Revision: "1", Filename: "ordinary.wav"})
	require.NoError(t, err)
	transcript := []byte("ordinary exact supplied transcript\n")
	transcriptIdentity := contentIdentity(transcript)
	artifact, err := vault.ImportRecordingArtifact(t.Context(), MediaArtifactRequest{
		OperationID: "00000000-0000-4000-8000-000000000433", SourceID: mediaReceipt.SourceID,
		OccurrenceID: mediaReceipt.OccurrenceID, Kind: "transcript", Origin: "supplied",
		Filename: "ordinary.txt", MediaType: "text/plain", SHA256: transcriptIdentity.SHA256,
		ByteLength: transcriptIdentity.Size, Content: bytes.NewReader(transcript),
	})
	require.NoError(t, err)
	require.NotEmpty(t, artifact.SuppliedInputID)
	node, err := vault.Stat(t.Context(), "/media/"+identity.SHA256[:2]+"/"+identity.SHA256+".wav")
	require.NoError(t, err)
	selector := ProcessingSelector{NodeID: node.ID, ContentVersionID: mediaReceipt.ContentVersionID,
		Profile: "supplied-transcript"}
	plan, err := vault.PlanProcessing(t.Context(), ProcessingPlanRequest{Selector: selector})
	require.NoError(t, err)
	job, err := vault.StartProcessing(t.Context(), StartProcessingRequest{
		PlanRequest: ProcessingPlanRequest{Selector: selector}, PlanFingerprint: plan.Fingerprint, Consent: true,
	})
	require.NoError(t, err)
	require.NotEmpty(t, job.AttachmentID)
	_, err = vault.RevokeMediaOccurrence(t.Context(),
		"00000000-0000-4000-8000-000000000434", mediaReceipt.OccurrenceID, "1")
	require.NoError(t, err)
	rendered, err := vault.Rendition(t.Context(), RenditionRequest{Selector: selector})
	if err == nil {
		_, readErr := io.ReadAll(rendered.Reader)
		require.NoError(t, readErr)
		require.NoError(t, rendered.Reader.Verify())
		require.NoError(t, rendered.Reader.Close())
	}
	require.ErrorIs(t, err, ErrNotFound)
}
