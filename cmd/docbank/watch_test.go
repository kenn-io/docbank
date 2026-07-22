package main

import (
	"bytes"
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
		ScanInterval: "1m0s", Exclude: []string{"cache/"},
		Job: &api.Job{Status: "running"},
	}})
	require.NoError(t, err)
	assert.Contains(t, out.String(), `"/local/agent\nsessions"`)
	assert.Contains(t, out.String(), `"/archives/\x1b[31msessions"`)
	assert.NotContains(t, out.String(), "\x1b")
}

func TestWriteWatchedInboxesExplainsEmptyConfig(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, writeWatchedInboxes(&out, nil))
	assert.Equal(t, "no watched inboxes configured\n", out.String())
}
