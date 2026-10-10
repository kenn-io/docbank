package jobs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestStorageAdmissionDefersAndReclaimsSameOperation(t *testing.T) {
	for _, action := range []string{"resume", "cancel", "shutdown", "control error", "cancel with broken controls"} {
		t.Run(action, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "vault.db")
			metadata, err := store.Open(path)
			require.NoError(t, err)
			defer func() { require.NoError(t, metadata.Close()) }()
			operation, err := metadata.CreateLocalOperation(t.Context(), store.StorageOperationKindPhotoImport, `{"source_root":"example"}`)
			require.NoError(t, err)
			if action == "cancel with broken controls" {
				controls := filepath.Join(filepath.Dir(path), "lane-controls.json")
				require.NoError(t, os.WriteFile(controls, []byte("{"), 0o600))
				require.NoError(t, metadata.RequestStorageOperationCancel(t.Context(), operation.ID))
				cancelled, err := AdmitStorageOperation(t.Context(), metadata, operation.ID)
				require.NoError(t, err)
				require.True(t, cancelled)
				return
			}
			_, err = metadata.ClaimStorageOperation(t.Context(), operation.ID)
			require.NoError(t, err)
			require.NoError(t, metadata.AdvanceStorageOperation(t.Context(), operation.ID, "", 1, 0, 0, `{"added":1}`))
			control, err := metadata.SetLaneControl(t.Context(), store.LaneControl{Lane: operation.Kind, Paused: true, Concurrency: 1}, 1)
			require.NoError(t, err)
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			type result struct {
				cancelled bool
				err       error
			}
			done := make(chan result, 1)
			go func() {
				cancelled, err := AdmitStorageOperation(ctx, metadata, operation.ID)
				done <- result{cancelled, err}
			}()
			require.Eventually(t, func() bool {
				current, err := metadata.StorageOperation(t.Context(), operation.ID)
				return err == nil && current.State == store.StorageOperationQueued
			}, 10*time.Second, 10*time.Millisecond)
			current, err := metadata.StorageOperation(t.Context(), operation.ID)
			require.NoError(t, err)
			require.Empty(t, current.Error)
			require.Equal(t, int64(1), current.CompletedObjects)
			require.Equal(t, `{"added":1}`, current.ReceiptJSON)
			switch action {
			case "resume":
				control.Paused = false
				_, err = metadata.SetLaneControl(t.Context(), control, control.Revision)
				require.NoError(t, err)
			case "cancel":
				require.NoError(t, metadata.RequestStorageOperationCancel(t.Context(), operation.ID))
			case "shutdown":
				stop()
			case "control error":
				controls := filepath.Join(filepath.Dir(path), "lane-controls.json")
				require.NoError(t, os.WriteFile(controls, []byte("{"), 0o600))
				require.Eventually(t, func() bool {
					current, err := metadata.StorageOperation(t.Context(), operation.ID)
					return err == nil && current.State == store.StorageOperationQueued &&
						strings.Contains(current.Error, "waiting for readable lane controls")
				}, 10*time.Second, 10*time.Millisecond, "the waiting operation must say why")
				select {
				case got := <-done:
					t.Fatalf("unreadable controls ended admission: %v", got.err)
				default:
				}
				// Admission keeps polling; Windows readers deny deletion until their read closes.
				require.EventuallyWithT(t, func(c *assert.CollectT) {
					require.NoError(c, os.Remove(controls))
				}, 10*time.Second, 10*time.Millisecond)
			}
			got := <-done
			switch action {
			case "shutdown":
				require.ErrorIs(t, got.err, context.Canceled)
			default:
				require.NoError(t, got.err)
			}
			require.Equal(t, action == "cancel", got.cancelled)
			current, err = metadata.StorageOperation(t.Context(), operation.ID)
			require.NoError(t, err)
			if action == "shutdown" {
				require.Equal(t, store.StorageOperationQueued, current.State)
			} else {
				require.Equal(t, store.StorageOperationRunning, current.State)
				require.Empty(t, current.Error)
			}
		})
	}
}

func TestStorageAdmissionClearsUnreadableNoteAfterRestart(t *testing.T) {
	for _, test := range []struct {
		name, stored, want string
	}{
		{"earlier unreadable note", unreadableControlsReason + "decoding lane controls", ""},
		{"unrelated error", "placing blob failed", "placing blob failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				metadata, err := store.Open(filepath.Join(t.TempDir(), "vault.db"))
				require.NoError(t, err)
				defer func() { require.NoError(t, metadata.Close()) }()
				operation, err := metadata.CreateLocalOperation(
					t.Context(), store.StorageOperationKindPhotoImport, `{"source_root":"example"}`,
				)
				require.NoError(t, err)
				// A previous daemon run left this error; the controls now read but stay paused.
				require.NoError(t, metadata.NoteQueuedStorageOperation(t.Context(), operation.ID, test.stored))
				_, err = metadata.SetLaneControl(t.Context(),
					store.LaneControl{Lane: operation.Kind, Paused: true, Concurrency: 1}, 1)
				require.NoError(t, err)
				ctx, cancel := context.WithTimeout(t.Context(), 5*LanePollInterval)
				defer cancel()
				_, err = AdmitStorageOperation(ctx, metadata, operation.ID)
				require.ErrorIs(t, err, context.DeadlineExceeded)
				current, err := metadata.StorageOperation(t.Context(), operation.ID)
				require.NoError(t, err)
				require.Equal(t, store.StorageOperationQueued, current.State)
				require.Equal(t, test.want, current.Error)
			})
		})
	}
}
