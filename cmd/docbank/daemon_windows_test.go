//go:build windows

package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

func TestConfiguredWatchStoresAllowUnavailableSecondaryDrive(t *testing.T) {
	var unavailableRoot string
	for drive := 'Z'; drive >= 'A'; drive-- {
		candidate := string(drive) + `:\`
		if _, err := os.Stat(candidate); errors.Is(err, fs.ErrNotExist) {
			unavailableRoot = candidate
			break
		}
	}
	if unavailableRoot == "" {
		t.Skip("all Windows drive letters are available")
	}

	cfg := config.Default()
	cfg.Watches = []config.WatchConfig{{
		Name: "inbox", Source: t.TempDir(), Destination: "/inbox",
	}}
	cfg.StoreBindings = map[string]config.StoreBindingConfig{
		"archive": {Kind: "filesystem", Path: filepath.Join(unavailableRoot, "archive")},
	}
	stores := []store.BlobStore{{
		ID:   "10000000-0000-4000-8000-000000000001",
		Name: "archive", Kind: "filesystem", Role: "secondary",
		Lifecycle: "active", Binding: "archive",
	}}

	require.NoError(t, validateConfiguredWatchStores(cfg, stores))
}
