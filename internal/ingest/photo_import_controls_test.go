package ingest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestPhotoImportPreparationThrottlesAdmissionAndConsumesCancellation(t *testing.T) {
	ing := newTestIngester(t)
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "video.MP4"), bytes.Repeat([]byte("synthetic video"), 600000), 0o600))
	synctest.Test(t, func(t *testing.T) {
		inflight, admissions, cancellations := 0, 0, 0
		report, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{
			ActivityBegin: func() { inflight++ },
			ActivityEnd:   func() { inflight-- },
			Admit: func(context.Context) (bool, error) {
				admissions++
				require.Zero(t, inflight, "admission may wait on a paused lane")
				return false, nil
			},
			Mutate: func(_ context.Context, fn func() error) error {
				require.Equal(t, 1, inflight)
				return fn()
			},
			Progress: func(_ context.Context, done, _ int, _ store.PhotoImportReceipt) error {
				if done == 0 {
					time.Sleep(101 * time.Millisecond)
				}
				return nil
			},
			Cancelled: func(context.Context) (bool, error) { cancellations++; return false, nil },
		})
		require.NoError(t, err)
		require.Equal(t, int64(1), report.Receipt.Added)
		require.Equal(t, 3, admissions, "preparation admits once after the throttle interval, then the group boundary forces admission")
		require.Equal(t, 2, cancellations, "only destination and group commit recheck cancellation")
		require.Zero(t, inflight)
		report, err = ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{
			Admit: func(context.Context) (bool, error) { return true, nil },
			Cancelled: func(context.Context) (bool, error) {
				t.Fatal("admission already returned cancellation")
				return false, nil
			},
		})
		require.NoError(t, err)
		require.True(t, report.Cancelled)
	})
	t.Run("one settlement across six files", func(t *testing.T) {
		root := t.TempDir()
		for i := range 6 {
			require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("IMG_%d.JPG", i)), []byte(fmt.Sprint("jpeg-", i)), 0o600))
		}
		ing := newTestIngester(t)
		synctest.Test(t, func(t *testing.T) {
			begin, end := 0, 0
			interval := 500 * time.Millisecond
			var entered, left []time.Time
			report, err := ing.ImportPhotoDirectory(t.Context(), root, "/photos", PhotoImportOptions{
				SettleInterval: interval,
				ActivityBegin:  func() { begin++ },
				ActivityEnd:    func() { end++ },
				Mutate: func(_ context.Context, fn func() error) error {
					entered = append(entered, time.Now())
					defer func() { left = append(left, time.Now()) }()
					return fn()
				},
			})
			require.NoError(t, err)
			assert.Equal(t, int64(6), report.Receipt.Added)
			assert.Equal(t, 1, begin)
			assert.Equal(t, 1, end)
			require.Len(t, entered, 7)
			assert.Equal(t, interval, entered[1].Sub(left[0]))
			for i := 2; i < len(entered); i++ {
				assert.Equal(t, time.Duration(0), entered[i].Sub(left[i-1]))
			}
			for i := range entered {
				assert.Equal(t, entered[i], left[i], "settlement stays outside the mutation gate")
			}
		})
	})
}
