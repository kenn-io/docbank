package store

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLaneControlsDurableAndOutsideMetadata(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "vault.db")
	s, err := Open(path)
	require.NoError(t, err)
	control, err := s.LaneControl(t.Context(), VisualPreviewLane)
	require.NoError(t, err)
	require.Equal(t, LaneControl{Lane: VisualPreviewLane, Concurrency: 1, Revision: 1}, control)
	control.Paused, control.Concurrency = true, 3
	control, err = s.SetLaneControl(t.Context(), control, control.Revision)
	require.NoError(t, err)
	_, err = s.SetLaneControl(t.Context(), control, 1)
	require.ErrorIs(t, err, ErrStaleRevision)
	control.Paused = false
	control, err = s.SetLaneControl(t.Context(), control, control.Revision)
	require.NoError(t, err)
	require.Equal(t, int64(3), control.Revision)
	var metadata bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &metadata))
	assert.NotContains(t, metadata.String(), "lane")
	require.NoError(t, s.Close())
	s, err = Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()
	reopened, err := s.LaneControl(t.Context(), VisualPreviewLane)
	require.NoError(t, err)
	require.Equal(t, control, reopened)
	all, err := s.LaneControls(t.Context())
	require.NoError(t, err)
	require.Equal(t, control, all[VisualPreviewLane])
	require.Equal(t, LaneControl{Lane: "place", Concurrency: 1, Revision: 1}, all["place"])
}

func TestLaneControlsRejectMalformedFile(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"syntax":        "{",
		"null":          "null",
		"concurrency":   `{"derive:visual-previews":{"paused":false,"concurrency":9,"revision":2}}`,
		"revision":      `{"place":{"paused":true,"concurrency":1,"revision":1}}`,
		"pause typo":    `{"photo_import":{"pauzed":true,"concurrency":1,"revision":2}}`,
		"pause missing": `{"photo_import":{"concurrency":1,"revision":2}}`,
		"pause null":    `{"photo_import":{"paused":null,"concurrency":1,"revision":2}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			s, err := Open(filepath.Join(dir, "vault.db"))
			require.NoError(t, err)
			defer func() { require.NoError(t, s.Close()) }()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "lane-controls.json"), []byte(content), 0o600))
			_, err = s.LaneControl(t.Context(), VisualPreviewLane)
			require.ErrorContains(t, err, "lane-controls.json")
			require.ErrorIs(t, err, ErrLaneControlsFile)
			_, err = s.LaneControls(t.Context())
			require.ErrorContains(t, err, "lane-controls.json")
			_, err = s.SetLaneControl(t.Context(), LaneControl{Lane: "place", Concurrency: 1}, 1)
			require.ErrorContains(t, err, "lane-controls.json")
			unchanged, err := os.ReadFile(filepath.Join(dir, "lane-controls.json"))
			require.NoError(t, err)
			require.Equal(t, content, string(unchanged))
		})
	}
}

func TestLaneControlsIgnoreFieldsFromNewerReleases(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "vault.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()
	unknown := `{"paused":"automatic","concurrency":12,"revision":1,"priority":"high"}`
	content := `{"place":{"paused":true,"concurrency":1,"revision":2,"priority":"high"},"future-lane":` + unknown + `}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lane-controls.json"), []byte(content), 0o600))
	control, err := s.LaneControl(t.Context(), "place")
	require.NoError(t, err)
	assert.Equal(t, LaneControl{Lane: "place", Paused: true, Concurrency: 1, Revision: 2}, control)
	all, err := s.LaneControls(t.Context())
	require.NoError(t, err)
	assert.NotContains(t, all, "future-lane")
	control.Paused = false
	_, err = s.SetLaneControl(t.Context(), control, control.Revision)
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(dir, "lane-controls.json"))
	require.NoError(t, err)
	var entries map[string]jsontext.Value
	require.NoError(t, json.Unmarshal(data, &entries))
	assert.JSONEq(t, unknown, string(entries["future-lane"]))
}

func TestLaneControlsRejectReadOnlyAndInvalidLimits(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, lane := range []string{
		"rendition", "embedding", "export", "maintenance:gc", "storage:operation", "unknown",
	} {
		_, err := s.SetLaneControl(t.Context(), LaneControl{Lane: lane, Concurrency: 1}, 1)
		require.ErrorIs(t, err, ErrLaneControl)
	}
	for _, lane := range []string{
		VisualPreviewLane, "place", "evacuate", "repair", "salvage", StorageOperationKindPhotoImport,
	} {
		_, err := s.SetLaneControl(t.Context(), LaneControl{Lane: lane, Concurrency: 1}, 99)
		require.ErrorIs(t, err, ErrStaleRevision)
		for _, limit := range []int{0, 5} {
			_, err := s.SetLaneControl(t.Context(), LaneControl{Lane: lane, Concurrency: limit}, 1)
			require.ErrorIs(t, err, ErrLaneControl)
		}
		_, err = s.SetLaneControl(t.Context(), LaneControl{Lane: lane, Concurrency: 2}, 1)
		if lane != VisualPreviewLane {
			require.ErrorIs(t, err, ErrLaneControl)
		} else {
			require.NoError(t, err)
		}
	}
}

func TestRestoreLaneControlsResetOnlyOnPublication(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"paused", "malformed"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(filepath.Join(dir, "vault.db"))
			require.NoError(t, err)
			defer func() { require.NoError(t, s.Close()) }()
			_, err = s.SetLaneControl(t.Context(), LaneControl{Lane: VisualPreviewLane, Paused: true, Concurrency: 3}, 1)
			require.NoError(t, err)
			path := filepath.Join(dir, laneControlsFile)
			if name == "malformed" {
				require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
			}
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, FinishRestoreLaneControls(dir, false))
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			require.NoError(t, FinishRestoreLaneControls(dir, true))
			current, err := s.LaneControl(t.Context(), VisualPreviewLane)
			require.NoError(t, err)
			assert.Equal(t, LaneControl{Lane: VisualPreviewLane, Concurrency: 1, Revision: 1}, current)
		})
	}
}

func TestLaneControlsConcurrentReadsAndWrites(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "vault.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()
	done := make(chan error, 1)
	go func() {
		control := LaneControl{Lane: VisualPreviewLane, Concurrency: 1, Revision: 1}
		for range 300 {
			control.Paused = !control.Paused
			control, err = s.SetLaneControl(t.Context(), control, control.Revision)
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for range 300 {
		_, err := s.LaneControl(t.Context(), VisualPreviewLane)
		require.NoError(t, err)
		_, err = s.LaneControls(t.Context())
		require.NoError(t, err)
	}
	require.NoError(t, <-done)
}
