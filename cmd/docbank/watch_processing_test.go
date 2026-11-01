package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/config"
)

func TestDaemonRejectsWatchProfileWithoutRuntime(t *testing.T) {
	cfg, _ := mistralOCRProcessingConfig(t)
	profile := cfg.RenditionProfiles["documents"]
	profile.Runtime = nil
	cfg.RenditionProfiles["documents"] = profile
	cfg.Watches = []config.WatchConfig{{Name: "inbox", Source: t.TempDir(), Destination: "/inbox", Profile: "documents", CaptureOnChange: true, SettleTime: config.Duration(time.Second), ScanInterval: config.Duration(time.Second)}}
	require.NoError(t, cfg.Validate())
	root := t.TempDir()
	require.NoError(t, writeDaemonASRConfig(root, cfg))
	t.Setenv("DOCBANK_HOME", root)
	require.ErrorContains(t, runServe(t.Context()), `watch "inbox" profile "documents" has no executable runtime`)
}
