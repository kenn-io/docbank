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
		want       string
	}{
		{"no profile", nil, `-`},
		{"active", &api.WatchProcessingStatus{Profile: "mistral-ocr", ConsentState: "active"}, `mistral-ocr`},
		{"waiting", &api.WatchProcessingStatus{Profile: "mistral-ocr", ConsentState: "revoked"}, `mistral-ocr \(waiting on consent: revoked\)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			require.NoError(t, writeWatchedInboxes(&out, []api.WatchedInbox{
				{Name: "waiting", Processing: tc.processing, Job: &api.Job{Status: "running"}},
			}))
			assert.Regexp(t, `waiting\s+idle\s`, out.String())
			assert.Regexp(t, `EXCLUDES\s+ERROR\s+PROCESSING`, out.String())
			assert.Regexp(t, tc.want+`\s*$`, out.String())
		})
	}
}
