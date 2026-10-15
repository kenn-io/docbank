package api_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

func TestMediaM4AHTTPPublicationAndReuse(t *testing.T) {
	t.Parallel()
	ts, catalog := newTestServer(t, configureMediaTestService(t, 0))
	client := daemonconn.New(ts.URL, testAPIKey)
	data := mediatest.M4A("alac")
	hash := processingTestHash(string(data))
	metadata := api.MediaSuppliedMetadata{OperationID: "00000000-0000-4000-8000-000000000801", Filename: "call.m4a", MediaType: "audio/x-m4a", SHA256: hash, ByteLength: int64(len(data)), Occurrence: api.MediaOccurrenceBody{Ref: "first", Revision: "1"}}
	first, err := client.SubmitSuppliedMedia(t.Context(), metadata, bytes.NewReader(data))
	require.NoError(t, err)
	node, err := catalog.NodeByPath(t.Context(), "/media/"+hash[:2]+"/"+hash+".m4a")
	require.NoError(t, err)
	require.Equal(t, first.ContentVersionID, node.CurrentVersionID)
	version, err := catalog.ContentVersionByID(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	require.Equal(t, "audio/mp4", version.MimeType)
	metadata.OperationID = "00000000-0000-4000-8000-000000000802"
	metadata.MediaType = "audio/mp4"
	metadata.Occurrence.Ref = "second"
	second, err := client.SubmitSuppliedMedia(t.Context(), metadata, bytes.NewReader(data))
	require.NoError(t, err)
	require.Equal(t, first.SourceID, second.SourceID)
	require.Equal(t, first.ContentVersionID, second.ContentVersionID)
	require.NotEqual(t, first.OccurrenceID, second.OccurrenceID)
}
