package blob

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

func TestPlacementPauseAtObjectBoundaryThenCancel(t *testing.T) {
	metadata, blobs, runner, destination := placementTestVault(t)
	first, _ := placementTestFile(t, metadata, blobs, []byte("first image"))
	_, _, err := metadata.Move(t.Context(), first.ID, metadata.RootID(), "first.txt", first.Revision)
	require.NoError(t, err)
	placementTestFile(t, metadata, blobs, []byte("second image"))
	plan, err := metadata.PlanPlacement(t.Context(), store.PlacementRequest{TargetNodeID: metadata.RootID(), SourceStoreID: metadata.PrimaryBlobStoreID(), DestinationStoreID: destination.ID})
	require.NoError(t, err)
	require.Len(t, plan.Hashes, 2)
	id := createPlacementOperation(t, metadata, plan)
	_, err = metadata.ClaimStorageOperation(t.Context(), id)
	require.NoError(t, err)
	control, err := metadata.SetLaneControl(t.Context(), store.LaneControl{Lane: "place", Paused: true, Concurrency: 1}, 1)
	require.NoError(t, err)
	paused := false
	runner.Commit = func(fn func() error) error {
		if err := fn(); err != nil {
			return err
		}
		if !paused {
			paused = true
			current, err := metadata.LaneControl(t.Context(), "place")
			if err != nil {
				return err
			}
			current.Paused = true
			_, err = metadata.SetLaneControl(t.Context(), current, current.Revision)
			return err
		}
		return nil
	}
	supervisor := jobs.New(t.Context(), nil)
	defer func() { require.NoError(t, supervisor.Shutdown(context.Background())) }()
	require.NoError(t, runner.Start(supervisor, id))
	require.Eventually(t, func() bool {
		current, err := metadata.StorageOperation(t.Context(), id)
		return err == nil && current.State == store.StorageOperationQueued
	}, 10*time.Second, 10*time.Millisecond)
	current, err := metadata.StorageOperation(t.Context(), id)
	require.NoError(t, err)
	require.Zero(t, current.CompletedObjects)
	control.Paused = false
	_, err = metadata.SetLaneControl(t.Context(), control, control.Revision)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		current, err := metadata.StorageOperation(t.Context(), id)
		return err == nil && current.State == store.StorageOperationQueued && current.CompletedObjects == 1
	}, 10*time.Second, 10*time.Millisecond)
	current, err = metadata.StorageOperation(t.Context(), id)
	require.NoError(t, err)
	require.NotEmpty(t, current.ReceiptJSON)
	require.Empty(t, current.Error)
	require.NoError(t, metadata.RequestStorageOperationCancel(t.Context(), id))
	require.Eventually(t, func() bool {
		current, err := metadata.StorageOperation(t.Context(), id)
		return err == nil && current.State == store.StorageOperationCancelled && current.CompletedObjects == 1
	}, 10*time.Second, 10*time.Millisecond)
}
