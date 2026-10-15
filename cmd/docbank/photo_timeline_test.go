package main

import (
	"encoding/json/v2"
	"image/color"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
)

func TestPhotoTimelineCLICountsPhotos(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "timeline-image.jpeg", string(mediatest.JPEG(2, 2, color.White)))
	_, err := runCLI(t, "add", source, "--dest", "/inbox")
	require.NoError(t, err)

	out, err := runCLI(t, "photos", "timeline")
	require.NoError(t, err, out)
	var timeline api.PhotoTimeline
	require.NoError(t, json.Unmarshal([]byte(out), &timeline), out)
	require.Len(t, timeline.Facets, 1)
	facet := timeline.Facets[0]
	require.True(t, facet.Available)
	assert.Equal(t, int64(1), *facet.Total)
	assert.Zero(t, *facet.Missing, "the file modified time places a photo without EXIF")

	out, err = runCLI(t, "photos", "timeline", "--query", `{"filters":{"capture_before":"2000-01-01"}}`)
	require.NoError(t, err, out)
	require.NoError(t, json.Unmarshal([]byte(out), &timeline), out)
	assert.Equal(t, int64(0), *timeline.Facets[0].Total)

	_, err = runCLI(t, "photos", "timeline", "--query", `{"unknown":1}`)
	require.Error(t, err)
	assert.Equal(t, exitUsage, commandExitCode(err, true))
}
