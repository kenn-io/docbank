//go:build !windows

package processing

import (
	"bytes"
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media/mediatest"
)

type exhaustedMediaReader struct{}

func (exhaustedMediaReader) Read([]byte) (int, error) {
	return 0, &os.PathError{Op: "write", Path: "synthetic", Err: syscall.ENOSPC}
}

func TestMediaDiskExhaustionIsRetryableAndCleansStage(t *testing.T) {
	t.Parallel()
	f := newPublicationFixture(t)
	spool := t.TempDir()
	service, err := NewService(ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: newWorkerTestGate(), SpoolDirectory: spool})
	require.NoError(t, err)
	raw := mediatest.WAV()
	_, err = service.StageMediaContent(t.Context(), exhaustedMediaReader{}, int64(len(raw)), 4<<30, processingHash(string(raw)))
	require.ErrorIs(t, err, ErrMediaStagingUnavailable)
	require.Zero(t, service.mediaStagedBytes)
	files, err := os.ReadDir(spool)
	require.NoError(t, err)
	require.Empty(t, files)
	require.ErrorIs(t, mediaStagingError(errors.Join(errors.New("sync failed"), syscall.EDQUOT)), ErrMediaStagingUnavailable)
	staged, err := service.StageMediaContent(t.Context(), bytes.NewReader(raw), int64(len(raw)), 4<<30, processingHash(string(raw)))
	require.NoError(t, err)
	require.NoError(t, staged.Close())
}
