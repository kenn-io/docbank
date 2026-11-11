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
		var request struct {
			Run bool `json:"run"`
		}
		require.NoError(t, json.UnmarshalRead(r.Body, &request))
		run = request.Run
		w.Header().Set("Content-Type", "application/json")
		report := api.GCReport{CandidateBlobs: 1, PendingPackedBlobs: 1, PendingPackedBytes: 100}
		if run {
			report.Removed, report.RemovedBlobs, report.Run = 1, 1, true
		}
		assert.NoError(t, json.MarshalWrite(w, report))
	})

	out, err := runCLI(t, "vault", "gc")
	require.NoError(t, err)
	assert.Contains(t, out, "1 packed blob(s), 100 stored byte(s) pending repack")
	assert.NotContains(t, out, "docbank storage repack")

	out, err = runCLI(t, "vault", "gc", "--run")
	require.NoError(t, err)
	assert.Contains(t, out, "repack: docbank storage repack")
}
