package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/api"
)

func TestWriteWatchedInboxesEscapesMachineAndVirtualPaths(t *testing.T) {
	var out bytes.Buffer
	err := writeWatchedInboxes(&out, []api.WatchedInbox{{
		Name: "sessions", Source: "/local/agent\nsessions",
		Destination: "/archives/\x1b[31msessions", SettleTime: "1h0m0s",
		MinimumAge: "168h0m0s", ScanInterval: "1m0s", Exclude: []string{"cache/"}, ExcludePatterns: []string{"*.swp", "*.tmp"}, CaptureOnChange: true,
		Tags: []string{"receipts", "line\nbreak"}, TagError: `tag "receipts" is unavailable`,
		Job: &api.Job{Status: "running"},
	}})
	require.NoError(t, err)
	assert.Contains(t, out.String(), `"/local/agent\nsessions"`)
	assert.Contains(t, out.String(), `"/archives/\x1b[31msessions"`)
	assert.NotContains(t, out.String(), "\x1b")
	fields := strings.Fields(strings.Split(strings.TrimSpace(out.String()), "\n")[1])
	assert.Equal(t, "3", fields[len(fields)-3])
	assert.Contains(t, out.String(), `["receipts" "line\nbreak"] (tag "receipts" is unavailable)`)
	assert.Contains(t, out.String(), "CAPTURE")
	assert.Contains(t, out.String(), "prompt")
}

func TestWriteWatchedInboxesExplainsEmptyConfig(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, writeWatchedInboxes(&out, nil))
	assert.Equal(t, "no watched inboxes configured\n", out.String())
}

func TestWriteWatchedInboxesDisplaysActivity(t *testing.T) {
	for _, tc := range []struct {
		name       string
		processing *api.WatchProcessingStatus
		routes     []api.WatchProcessingStatus
		want       string
	}{
		{name: "no profile", want: `-`},
		{name: "active", processing: &api.WatchProcessingStatus{Profile: "mistral-ocr", ConsentState: "active"}, want: `mistral-ocr`},
		{name: "waiting", processing: &api.WatchProcessingStatus{Profile: "mistral-ocr", ConsentState: "revoked"}, want: `mistral-ocr \(waiting on consent: revoked\)`},
		{name: "routes", routes: []api.WatchProcessingStatus{
			{MediaFamily: "audio_video", Profile: "asr", ConsentState: "required"},
			{MediaFamily: "document", Profile: "docling", ConsentState: "active"},
			{MediaFamily: "image", Profile: "ocr", Error: "Consent status unavailable"},
		}, want: `audio_video=asr \(waiting on consent: required\), document=docling, image=ocr \(Consent status unavailable\)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			require.NoError(t, writeWatchedInboxes(&out, []api.WatchedInbox{
				{Name: "waiting", Processing: tc.processing, ProcessingRoutes: tc.routes, Job: &api.Job{Status: "running"}},
			}))
			assert.Regexp(t, `waiting\s+idle\s`, out.String())
			assert.Regexp(t, `EXCLUDES\s+ERROR\s+PROCESSING`, out.String())
			assert.Regexp(t, tc.want+`\s*$`, out.String())
		})
	}
}
