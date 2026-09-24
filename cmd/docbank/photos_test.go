package main

import (
	"encoding/json/v2"
	"fmt"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotosCLIEnrollsAddedImage(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "synthetic-image.jpeg", "synthetic image bytes")
	_, err := runCLI(t, "add", source, "--dest", "/inbox")
	require.NoError(t, err)
	c, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	node, err := c.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/inbox/synthetic-image.jpeg"}})
	require.NoError(t, err)
	assetFromNode, err := c.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	for _, selector := range []string{assetFromNode.ID, "id:" + strconv.FormatInt(node.ID, 10), "/inbox/synthetic-image.jpeg"} {
		out, err := runCLI(t, "photos", "assets", "inspect", selector)
		require.NoError(t, err, selector)
		var asset api.PhotoAsset
		require.NoError(t, json.Unmarshal([]byte(out), &asset))
		assert.Equal(t, assetFromNode.ID, asset.ID, selector)
		assert.Equal(t, int64(1), asset.Revision)
		require.Len(t, asset.Files, 1)
		assert.Equal(t, node.ID, asset.Files[0].NodeID)
	}
}

func TestPhotosCLIWorkflow(t *testing.T) {
	paths := [][]string{
		{"photos", "assets", "create"},
		{"photos", "assets", "inspect"},
		{"photos", "assets", "attach"},
		{"photos", "assets", "detach"},
		{"photos", "assets", "exclude"},
		{"photos", "assets", "promote"},
		{"photos", "assets", "display"},
		{"photos", "settings", "show"},
		{"photos", "settings", "set"},
		{"photos", "settings", "reset"},
	}
	for _, path := range paths {
		command, _, err := rootCmd.Find(path)
		require.NoError(t, err, path)
		assert.NotNil(t, command, path)
	}
	for _, path := range [][]string{
		{"photos", "assets", "attach"}, {"photos", "assets", "detach"},
		{"photos", "assets", "exclude"}, {"photos", "assets", "display"},
		{"photos", "settings", "set"}, {"photos", "settings", "reset"},
	} {
		command, _, err := rootCmd.Find(path)
		require.NoError(t, err)
		assert.NotNil(t, command.Flags().Lookup("revision"), path)
	}

	_ = setupVaultHome(t)
	source := writeSourceFile(t, "workflow-image.jpeg", "synthetic image bytes")
	_, err := runCLI(t, "add", source, "--dest", "/inbox")
	require.NoError(t, err)
	c, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	node, err := c.API().ResolvePath(t.Context(), &apiclient.ResolvePathRequestOptions{Query: &apiclient.ResolvePathQuery{Path: "/inbox/workflow-image.jpeg"}})
	require.NoError(t, err)
	asset, err := c.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)

	inspect, err := runCLI(t, "photos", "assets", "inspect", asset.ID)
	require.NoError(t, err)
	var inspected api.PhotoAsset
	require.NoError(t, json.Unmarshal([]byte(inspect), &inspected))
	assert.Equal(t, asset.ID, inspected.ID)

	display, err := runCLI(t, "photos", "assets", "display", asset.ID)
	require.NoError(t, err)
	var displayed api.PhotoAsset
	require.NoError(t, json.Unmarshal([]byte(display), &displayed))
	assert.Equal(t, asset.Revision, displayed.Revision)

	settingsOutput, err := runCLI(t, "photos", "settings", "show")
	require.NoError(t, err)
	var settings api.PhotoSettings
	require.NoError(t, json.Unmarshal([]byte(settingsOutput), &settings))
	reset, err := runCLI(t, "photos", "settings", "reset")
	require.NoError(t, err)
	var resetSettings api.PhotoSettings
	require.NoError(t, json.Unmarshal([]byte(reset), &resetSettings))
	assert.Equal(t, settings.Revision, resetSettings.Revision)

	excludedOutput, err := runCLI(t, "photos", "assets", "exclude", asset.ID, "--revision", strconv.FormatInt(asset.Revision, 10))
	require.NoError(t, err)
	var excluded api.PhotoAsset
	require.NoError(t, json.Unmarshal([]byte(excludedOutput), &excluded))
	assert.NotNil(t, excluded.ExcludedAt)
	_, err = runCLI(t, "photos", "assets", "exclude", asset.ID, "--excluded=false", "--revision", strconv.FormatInt(asset.Revision, 10))
	require.ErrorIs(t, err, store.ErrPhotoAssetRevision)
	assert.Equal(t, exitStale, commandExitCode(err, true))
	promotedOutput, err := runCLI(t, "photos", "assets", "promote", "/inbox/workflow-image.jpeg")
	require.NoError(t, err)
	var promoted api.PhotoAsset
	require.NoError(t, json.Unmarshal([]byte(promotedOutput), &promoted))
	assert.Equal(t, asset.ID, promoted.ID)
	assert.Nil(t, promoted.ExcludedAt)
}

func TestWithPhotoRevisionRetriesOnceOnlyWhenInferred(t *testing.T) {
	stale := fmt.Errorf("asset moved on: %w", store.ErrPhotoAssetRevision)
	run := func(args []string, failures int) (reads int, writes []int64, err error) {
		cmd := &cobra.Command{RunE: func(cmd *cobra.Command, _ []string) error {
			current := func() (*int64, error) {
				reads++
				revision := int64(10 + reads)
				return &revision, nil
			}
			_, err := withPhotoRevision(cmd, current, func(revision *int64) (struct{}, error) {
				writes = append(writes, *revision)
				if len(writes) <= failures {
					return struct{}{}, stale
				}
				return struct{}{}, nil
			})
			return err
		}}
		cmd.Flags().Int64Var(&photoRevision, "revision", 0, "")
		cmd.SetArgs(args)
		t.Cleanup(func() { photoRevision = 0 })
		return reads, writes, cmd.Execute()
	}

	reads, writes, err := run(nil, 1)
	require.NoError(t, err)
	assert.Equal(t, 2, reads)
	assert.Equal(t, []int64{11, 12}, writes)

	reads, writes, err = run(nil, 2)
	require.ErrorIs(t, err, store.ErrPhotoAssetRevision)
	assert.Equal(t, 2, reads)
	assert.Equal(t, []int64{11, 12}, writes)

	reads, writes, err = run([]string{"--revision", "7"}, 1)
	require.ErrorIs(t, err, store.ErrPhotoAssetRevision)
	assert.Zero(t, reads)
	assert.Equal(t, []int64{7}, writes)
}
