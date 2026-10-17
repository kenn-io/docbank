package checkout

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/jobs"
	"go.kenn.io/docbank/internal/store"
)

func TestCheckoutSettlementStartsWhenEachFileIsObserved(t *testing.T) {
	for _, trigger := range []string{"before open", "mid digest"} {
		t.Run(trigger, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := checkoutService(t)
				content := "original"
				if trigger == "mid digest" {
					content = strings.Repeat("a", 8<<20)
				}
				asset := checkoutPhoto(t, s, "photo.jpg", content)
				c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}})
				path := filepath.Join(c.Destination, filepath.FromSlash(entries[0].Path))
				before, err := os.Stat(path)
				require.NoError(t, err)
				var hashedAtEdit int64
				hooks := scanHooks{before: func(_ context.Context, _ string) error {
					// Admission can pause traversal before the file is opened.
					time.Sleep(3 * time.Second)
					return os.WriteFile(path, []byte("edited"), 0o600)
				}}
				if trigger == "mid digest" {
					hooks = scanHooks{progress: func(_ context.Context, n int64, _ bool) error {
						if n <= 0 || hashedAtEdit > 0 {
							return nil
						}
						hashedAtEdit = n
						time.Sleep(3 * time.Second)
						file, err := os.OpenFile(path, os.O_WRONLY, 0)
						if err != nil {
							return err
						}
						_, writeErr := file.WriteAt([]byte("b"), before.Size()-1)
						closeErr := file.Close()
						if writeErr != nil || closeErr != nil {
							return errors.Join(writeErr, closeErr)
						}
						return os.Chtimes(path, before.ModTime(), before.ModTime())
					}}
				}
				_, err = s.scanWorking(t.Context(), c, nil, nil, hooks)
				require.NoError(t, err)
				result, err := s.Store.PhotoCheckoutStatus(t.Context(), c.ID, "", 100)
				require.NoError(t, err)
				require.Equal(t, "pending", result.Entries[0].WorkingState)
				if trigger == "mid digest" {
					require.Positive(t, hashedAtEdit)
					require.Less(t, hashedAtEdit, before.Size(), "the final byte must still be unread")
					after, err := os.Stat(path)
					require.NoError(t, err)
					require.True(t, os.SameFile(before, after))
					require.Equal(t, before.Size(), after.Size())
					require.Equal(t, before.ModTime(), after.ModTime())
				}
				followup, err := s.scanCheckout(t.Context(), c.ID)
				require.NoError(t, err)
				require.Equal(t, result.Entries[0].ObservedHash, followup.Entries[0].ObservedHash)
				require.Equal(t, "pending", followup.Entries[0].WorkingState, "a new edit must receive its own settlement window")
				time.Sleep(2 * time.Second)
				result, err = s.scanCheckout(t.Context(), c.ID)
				require.NoError(t, err)
				require.Equal(t, "edited", result.Entries[0].WorkingState)
			})
		})
	}
}

func TestCheckoutRestartAfterFinalizationPreservesCancellationRules(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		kind   string
		paused bool
		bound  bool
	}{
		{"scan", store.StorageOperationKindPhotoCheckoutScan, false, false},
		{"commit", store.StorageOperationKindPhotoCheckoutCommit, false, false},
		{"paused scan", store.StorageOperationKindPhotoCheckoutScan, true, false},
		{"bound rebind", store.StorageOperationKindPhotoCheckoutRebind, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := checkoutService(t)
			asset := checkoutPhoto(t, s, "photo.jpg", "original")
			c, _ := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}})
			destination := ""
			if tc.bound {
				destination = c.Destination
				var snapshot bytes.Buffer
				require.NoError(t, s.Store.ExportMetadata(t.Context(), &snapshot))
				s = checkoutService(t)
				require.NoError(t, s.Store.ImportMetadata(t.Context(), bytes.NewReader(snapshot.Bytes())))
			}
			c, op, err := s.PrepareOperation(t.Context(), c.ID, tc.kind, destination)
			require.NoError(t, err)
			if tc.bound {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				interrupted := false
				s.Mutate = func(ctx context.Context, fn func() error) error {
					if err := fn(); err != nil {
						return err
					}
					current, err := s.Store.PhotoCheckout(ctx, c.ID)
					if err == nil && current.DestinationReserved && !interrupted {
						interrupted = true
						cancel()
					}
					return err
				}
				require.ErrorIs(t, s.RunInspection(ctx, c), context.Canceled)
				require.True(t, interrupted)
				s.Mutate = nil
			} else {
				_, err = s.Store.ClaimStorageOperation(t.Context(), op.ID)
				require.NoError(t, err)
				cancelled, err := s.Store.FinalizeLocalOperation(t.Context(), op.ID)
				require.NoError(t, err)
				require.False(t, cancelled)
			}
			interrupted, err := s.Store.StorageOperation(t.Context(), op.ID)
			require.NoError(t, err)
			require.Equal(t, "@finalizing", interrupted.Cursor)
			requested := false
			if tc.paused {
				_, err = s.Store.SetLaneControl(t.Context(), store.LaneControl{Lane: "photo_checkout", Paused: true, Concurrency: 1}, 1)
				require.NoError(t, err)
			} else {
				admissions := 0
				s.admit = func(ctx context.Context, metadata *store.Store, id string) (bool, error) {
					admissions++
					if admissions == 2 {
						current, err := metadata.StorageOperation(ctx, id)
						require.NoError(t, err)
						require.Empty(t, current.Cursor, "claiming resumed verification must clear finalization")
						require.NoError(t, metadata.RequestStorageOperationCancel(ctx, id), "resumed verification must accept cancellation without pausing")
						requested = true
					}
					return jobs.AdmitStorageOperation(ctx, metadata, id)
				}
			}
			supervisor := jobs.New(t.Context(), nil)
			t.Cleanup(func() { require.NoError(t, supervisor.Shutdown(context.Background())) })
			require.NoError(t, s.Resume(t.Context(), supervisor))
			if tc.paused {
				require.Eventually(t, func() bool {
					current, err := s.Store.StorageOperation(t.Context(), op.ID)
					return err == nil && current.State == store.StorageOperationQueued
				}, 3*time.Second, 10*time.Millisecond)
				cancelErr := s.Store.RequestStorageOperationCancel(t.Context(), op.ID)
				if tc.bound {
					require.ErrorIs(t, cancelErr, store.ErrStorageOperationTerminal)
					control, err := s.Store.LaneControl(t.Context(), "photo_checkout")
					require.NoError(t, err)
					control.Paused = false
					_, err = s.Store.SetLaneControl(t.Context(), control, control.Revision)
					require.NoError(t, err)
				} else {
					require.NoError(t, cancelErr, "resumed inspection must accept cancellation while paused")
				}
			}
			require.Eventually(t, func() bool {
				registered := supervisor.Snapshot()
				return len(registered) == 1 && registered[0].FinishedAt != nil
			}, 5*time.Second, 10*time.Millisecond)
			if !tc.paused {
				require.True(t, requested)
			}
			final, err := s.Store.StorageOperation(t.Context(), op.ID)
			require.NoError(t, err)
			if tc.bound {
				require.Equal(t, store.StorageOperationCompleted, final.State)
				bound, err := s.Store.PhotoCheckout(t.Context(), c.ID)
				require.NoError(t, err)
				require.Equal(t, destination, bound.Destination)
				require.True(t, bound.DestinationReserved)
			} else {
				require.Equal(t, store.StorageOperationCancelled, final.State)
			}
			_, total, err := s.Store.ContentVersions(t.Context(), asset.Files[0].NodeID, 100, 0)
			require.NoError(t, err)
			require.Equal(t, 1, total)
		})
	}
}

func TestCheckoutInspectionProgressCancellationAndRestart(t *testing.T) {
	t.Parallel()
	for _, rebind := range []bool{false, true} {
		for _, cancelJob := range []bool{false, true} {
			if !rebind && cancelJob {
				continue
			}
			t.Run(map[bool]string{false: "scan", true: "rebind"}[rebind]+map[bool]string{false: " restart", true: " cancel"}[cancelJob], func(t *testing.T) {
				s := checkoutService(t)
				asset := checkoutPhoto(t, s, "photo.jpg", strings.Repeat("a", 8<<20))
				c, _ := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}})
				destination := ""
				if rebind {
					destination = c.Destination
					var snapshot bytes.Buffer
					require.NoError(t, s.Store.ExportMetadata(t.Context(), &snapshot))
					s = checkoutService(t)
					require.NoError(t, s.Store.ImportMetadata(t.Context(), bytes.NewReader(snapshot.Bytes())))
				}
				c, op, err := s.PrepareOperation(t.Context(), c.ID, map[bool]string{true: store.StorageOperationKindPhotoCheckoutScan, false: store.StorageOperationKindPhotoCheckoutRebind}[destination == ""], destination)
				require.NoError(t, err)
				admissions := 0
				s.admit = func(ctx context.Context, metadata *store.Store, id string) (bool, error) {
					admissions++
					if admissions == 2 {
						return false, errors.New("database is busy")
					}
					return jobs.AdmitStorageOperation(ctx, metadata, id)
				}
				require.ErrorIs(t, s.RunInspection(t.Context(), c), jobs.ErrStorageOperationDeferred)
				deferred, err := s.Store.StorageOperation(t.Context(), op.ID)
				require.NoError(t, err)
				require.Equal(t, store.StorageOperationQueued, deferred.State)
				s.admit = nil
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var observed bool
				s.Mutate = func(ctx context.Context, fn func() error) error {
					err := fn()
					current, readErr := s.Store.StorageOperation(ctx, op.ID)
					require.NoError(t, readErr)
					var progress ScanResult
					require.NoError(t, json.Unmarshal([]byte(current.ReceiptJSON), &progress))
					if progress.HashedBytes > 0 && progress.HashedBytes < 8<<20 && !observed {
						observed = true
						require.Zero(t, current.CompletedObjects, "bytes must appear before the file finishes")
						if cancelJob {
							require.NoError(t, s.Store.RequestStorageOperationCancel(ctx, op.ID))
						} else {
							cancel()
						}
					}
					return err
				}
				err = s.RunInspection(ctx, c)
				require.True(t, observed)
				s.Mutate = nil
				if cancelJob {
					require.NoError(t, err)
					final, err := s.Store.StorageOperation(t.Context(), op.ID)
					require.NoError(t, err)
					require.Equal(t, store.StorageOperationCancelled, final.State)
					if rebind {
						restored, err := s.Store.PhotoCheckout(t.Context(), c.ID)
						require.NoError(t, err)
						require.Equal(t, "unbound", restored.State)
					}
				} else {
					require.ErrorIs(t, err, context.Canceled)
					require.NoError(t, s.RunInspection(t.Context(), c))
					final, err := s.Store.StorageOperation(t.Context(), op.ID)
					require.NoError(t, err)
					require.Equal(t, store.StorageOperationCompleted, final.State)
					var receipt ScanResult
					require.NoError(t, json.Unmarshal([]byte(final.ReceiptJSON), &receipt))
					require.Equal(t, int64(8<<20), receipt.HashedBytes)
					require.Equal(t, int64(1), receipt.Files)
					status, err := s.Store.PhotoCheckoutStatus(t.Context(), c.ID, "", 100)
					require.NoError(t, err)
					require.Len(t, status.Entries, 1)
					require.Equal(t, "clean", status.Entries[0].WorkingState)
				}
			})
		}
	}
}

func TestCheckoutInspectionCancellationFinalizesRetirement(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"queued", "paused", "running", "start failure"} {
		t.Run(state, func(t *testing.T) {
			s := checkoutService(t)
			asset := checkoutPhoto(t, s, "photo.jpg", "original")
			c, _ := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{asset.ID}})
			c, op, err := s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutScan, "")
			require.NoError(t, err)
			if state == "paused" {
				_, err = s.Store.SetLaneControl(t.Context(), store.LaneControl{Lane: "photo_checkout", Paused: true, Concurrency: 1}, 1)
				require.NoError(t, err)
			}
			if state == "running" {
				_, err = s.Store.ClaimStorageOperation(t.Context(), op.ID)
				require.NoError(t, err)
			}
			_, err = s.Store.RetirePhotoCheckout(t.Context(), c.ID)
			require.NoError(t, err)
			if state == "start failure" {
				supervisor := jobs.New(t.Context(), nil)
				require.NoError(t, supervisor.Shutdown(t.Context()))
				require.Error(t, s.StartOperation(supervisor, c))
				final, err := s.Store.StorageOperation(t.Context(), op.ID)
				require.NoError(t, err)
				require.Equal(t, store.StorageOperationFailed, final.State)
			} else {
				require.NoError(t, s.Store.RequestStorageOperationCancel(t.Context(), op.ID))
				require.NoError(t, s.RunInspection(t.Context(), c))
			}
			retired, err := s.Store.PhotoCheckout(t.Context(), c.ID)
			require.NoError(t, err)
			require.NotNil(t, retired.RetiredAt)
			require.True(t, retired.RetentionReleased)
		})
	}
}

func TestCheckoutInspectionBoundsReceiptsAndSkipsMissingCommits(t *testing.T) {
	t.Parallel()
	s := checkoutService(t)
	first := checkoutPhoto(t, s, "first.jpg", "first")
	second := checkoutPhoto(t, s, "second.jpg", "second")
	c, entries := completedCheckout(t, s, store.PhotoCheckoutSelection{AssetIDs: []string{first.ID, second.ID}})
	for _, entry := range entries {
		require.NoError(t, os.WriteFile(filepath.Join(c.Destination, filepath.FromSlash(entry.Path)), []byte("edited"), 0o600))
	}
	for i := range 101 {
		require.NoError(t, os.WriteFile(filepath.Join(c.Destination, fmt.Sprintf("untracked-%03d.jpg", i)), []byte("untracked"), 0o600))
	}
	c, op, err := s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutScan, "")
	require.NoError(t, err)
	require.NoError(t, s.RunInspection(t.Context(), c))
	final, err := s.Store.StorageOperation(t.Context(), op.ID)
	require.NoError(t, err)
	require.Equal(t, int64(2), final.CompletedObjects)
	var receipt ScanResult
	require.NoError(t, json.Unmarshal([]byte(final.ReceiptJSON), &receipt))
	require.Len(t, receipt.Untracked, 100)
	require.Equal(t, int64(101), receipt.Results["untracked"])
	require.Len(t, receipt.Paths, 2)
	require.Less(t, len(final.ReceiptJSON), 16*1024)
	for i := 101; i < 151; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(c.Destination, fmt.Sprintf("untracked-%03d.jpg", i)), []byte("untracked"), 0o600))
	}
	c, op, err = s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutScan, "")
	require.NoError(t, err)
	require.NoError(t, s.RunInspection(t.Context(), c))
	grown, err := s.Store.StorageOperation(t.Context(), op.ID)
	require.NoError(t, err)
	require.Less(t, len(grown.ReceiptJSON), 16*1024)
	require.NoError(t, json.Unmarshal([]byte(grown.ReceiptJSON), &receipt))
	require.Len(t, receipt.Untracked, 100)
	require.Equal(t, int64(151), receipt.Results["untracked"])
	require.NoError(t, os.Remove(filepath.Join(c.Destination, filepath.FromSlash(entries[0].Path))))
	_, err = s.scanCheckout(t.Context(), c.ID)
	require.NoError(t, err)
	missing := 0
	status, err := s.Store.PhotoCheckoutStatus(t.Context(), c.ID, "", 100)
	require.NoError(t, err)
	for _, e := range status.Entries {
		if e.WorkingState == "missing" {
			missing++
		}
	}
	require.Equal(t, 1, missing)
	c, _, err = s.PrepareOperation(t.Context(), c.ID, store.StorageOperationKindPhotoCheckoutCommit, "")
	require.NoError(t, err)
	require.NoError(t, s.RunCommit(t.Context(), c))
	nodeID := first.Files[0].NodeID
	if entries[0].AssetID == second.ID {
		nodeID = second.Files[0].NodeID
	}
	_, total, err := s.Store.ContentVersions(t.Context(), nodeID, 100, 0)
	require.NoError(t, err)
	require.Equal(t, 1, total, "missing files must not create versions")
}
