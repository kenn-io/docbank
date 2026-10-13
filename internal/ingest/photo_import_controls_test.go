package ingest

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPhotoImportPreparationThrottlesAdmissionAndConsumesCancellation(t *testing.T) {
	ing := newTestIngester(t)
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "video.MP4"), bytes.Repeat([]byte("synthetic video"), 600000), 0o600))
	inflight, admissions, cancellations := 0, 0, 0
	started := time.Now()
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
		Cancelled: func(context.Context) (bool, error) { cancellations++; return false, nil },
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), report.Receipt.Added)
	require.LessOrEqual(t, admissions, int(time.Since(started)/(100*time.Millisecond))+2)
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
}
