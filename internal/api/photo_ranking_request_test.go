package api

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

func TestSavedPhotoRankingMatchesLiveRequest(t *testing.T) {
	t.Parallel()
	value, err := query.Parse([]byte(`{"mode":"semantic","text":"dog","sort":{"field":"capture_time"}}`))
	require.NoError(t, err)
	coverage := store.CoverageSelection{Configuration: "synthetic"}
	request := photoRankingRequest(photoRankingQuery(value), false, coverage)
	require.Equal(t, query.Sort{Field: "relevance", Direction: "desc"}, request.Query.Sort)
	require.Equal(t, processing.SourceMetadataExtractorFingerprint, request.SourceMetadataFingerprint)
	require.Equal(t, coverage, request.Coverage)
	require.False(t, request.Hidden)
	value.Text = "  "
	require.Equal(t, query.ModeLexical, photoRankingQuery(value).Mode)
}
