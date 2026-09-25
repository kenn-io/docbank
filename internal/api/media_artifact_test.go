package api_test

import (
	"bytes"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestMediaArtifactImportReusesImmutableInputBeforeCreatingNodes(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, filename, mediaType, language string
		newOperation, conflict              bool
	}{
		{name: "operation filename", filename: "call.srt", conflict: true},
		{name: "operation MIME", mediaType: "text/vtt", conflict: true},
		{name: "new filename", filename: "call.srt", newOperation: true},
		{name: "changed language", filename: "call.srt", language: "fr", newOperation: true, conflict: true},
		{name: "changed MIME", filename: "call.srt", mediaType: "text/vtt", newOperation: true, conflict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ts, catalog := newTestServer(t, configureMediaTestService(t))
			c := daemonconn.New(ts.URL, testAPIKey)
			wav := mediatest.WAV()
			source, err := c.SubmitSuppliedMedia(t.Context(), api.MediaSuppliedMetadata{
				OperationID: "00000000-0000-4000-8000-000000000701", Filename: "call.wav",
				MediaType: "audio/wav", SHA256: processingTestHash(string(wav)), ByteLength: int64(len(wav)),
				Occurrence: api.MediaOccurrenceBody{Ref: "call", Revision: "1"},
			}, bytes.NewReader(wav))
			require.NoError(t, err)
			transcript := "synthetic transcript\n"
			metadata := api.MediaArtifactMetadata{
				OperationID: "00000000-0000-4000-8000-000000000702", OccurrenceID: source.OccurrenceID,
				Kind: "transcript", Filename: "call.txt", MediaType: "text/plain", Language: "en",
				SHA256: processingTestHash(transcript), ByteLength: int64(len(transcript)),
			}
			first, err := c.ImportMediaArtifact(t.Context(), source.SourceID, metadata, strings.NewReader(transcript))
			require.NoError(t, err)
			if test.newOperation {
				metadata.OperationID = "00000000-0000-4000-8000-000000000703"
			}
			if test.filename != "" {
				metadata.Filename = test.filename
			}
			if test.mediaType != "" {
				metadata.MediaType = test.mediaType
			}
			if test.language != "" {
				metadata.Language = test.language
			}
			replayed, err := c.ImportMediaArtifact(t.Context(), source.SourceID, metadata, strings.NewReader(transcript))
			if test.conflict {
				code, ok := daemonconn.ProblemCode(err)
				require.True(t, ok, "expected conflict, got %v", err)
				require.Equal(t, "operation_conflict", code)
			} else {
				require.NoError(t, err)
				require.Equal(t, first.ContentVersionID, replayed.ContentVersionID)
				require.Equal(t, first.SuppliedInputID, replayed.SuppliedInputID)
			}
			parent, err := catalog.NodeByPath(t.Context(), path.Join("/media-input", metadata.SHA256[:2]))
			require.NoError(t, err)
			children, err := catalog.Children(t.Context(), parent.ID)
			require.NoError(t, err)
			require.Len(t, children, 1, "a replay or conflict must not create another node")
		})
	}
}

func TestMediaArtifactConcurrentConflictsDoNotCreateExtraNodes(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureMediaTestService(t))
	c := daemonconn.New(ts.URL, testAPIKey)
	wav := mediatest.WAV()
	source, err := c.SubmitSuppliedMedia(t.Context(), api.MediaSuppliedMetadata{
		OperationID: "00000000-0000-4000-8000-000000000711", Filename: "call.wav",
		MediaType: "audio/wav", SHA256: processingTestHash(string(wav)), ByteLength: int64(len(wav)),
		Occurrence: api.MediaOccurrenceBody{Ref: "call", Revision: "1"},
	}, bytes.NewReader(wav))
	require.NoError(t, err)
	transcript := "synthetic concurrent transcript\n"
	digest := processingTestHash(transcript)
	requests := []api.MediaArtifactMetadata{
		{OperationID: "00000000-0000-4000-8000-000000000712", Filename: "call.txt", Language: "en"},
		{OperationID: "00000000-0000-4000-8000-000000000713", Filename: "call.srt", Language: "fr"},
	}
	start := make(chan struct{})
	errors := make([]error, len(requests))
	var workers sync.WaitGroup
	for index, request := range requests {
		workers.Go(func() {
			request.OccurrenceID, request.Kind, request.MediaType = source.OccurrenceID, "transcript", "text/plain"
			request.SHA256, request.ByteLength = digest, int64(len(transcript))
			<-start
			_, errors[index] = c.ImportMediaArtifact(t.Context(), source.SourceID, request, strings.NewReader(transcript))
		})
	}
	close(start)
	workers.Wait()
	succeeded := 0
	for _, err := range errors {
		if err == nil {
			succeeded++
			continue
		}
		code, ok := daemonconn.ProblemCode(err)
		require.True(t, ok)
		require.Equal(t, "operation_conflict", code)
	}
	require.Equal(t, 1, succeeded)
	parent, err := catalog.NodeByPath(t.Context(), path.Join("/media-input", digest[:2]))
	require.NoError(t, err)
	children, err := catalog.Children(t.Context(), parent.ID)
	require.NoError(t, err)
	require.Len(t, children, 1, "the losing import must not leave a node")
}
