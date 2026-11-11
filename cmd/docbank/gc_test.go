package main

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestGCPackedRepackGuidance(t *testing.T) {
	var run bool
	setupFakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/gc", r.URL.Path)
		var request struct {
			Run bool `json:"run"`
		}
		require.NoError(t, json.UnmarshalRead(r.Body, &request))
		run = request.Run
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, api.GCReport{CandidateBlobs: 1, PendingPackedBlobs: 1, PendingPackedBytes: 100}))
	})

	out, err := runCLI(t, "vault", "gc")
	require.NoError(t, err)
	assert.False(t, run)
	assert.Contains(t, out, "1 packed blob(s), 100 stored byte(s) pending repack")
	assert.NotContains(t, out, "docbank storage repack")
	assert.Contains(t, out, "dry run — pass --run to delete")

	out, err = runCLI(t, "vault", "gc", "--run")
	require.NoError(t, err)
	assert.True(t, run)
	assert.Contains(t, out, "1 packed blob(s), 100 stored byte(s) pending repack")
	assert.Contains(t, out, "docbank storage repack (see --help for thresholds)")
}
