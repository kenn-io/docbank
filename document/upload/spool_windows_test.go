//go:build windows

package upload

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/winsecurity"
)

func TestAuthorizeWindowsCreatesRestrictedSpoolFile(t *testing.T) {
	data := []byte("authoritative bytes\n")
	record := inspectCapability(t, data)
	directory := t.TempDir()
	upload, err := Authorize(t.Context(), Source{
		Reader: io.NopCloser(bytes.NewReader(data)), Directory: directory,
		testHook: func(stage authorizeStage, path string) error {
			if stage != authorizeStageWritten {
				return nil
			}
			file, openErr := winsecurity.OpenRestrictedCurrentUserFile(path)
			if openErr != nil {
				return openErr
			}
			return file.Close()
		},
	}, record, UploadMetadata{Filename: "notes.txt"})
	require.NoError(t, err)
	require.NoError(t, upload.Close())
	assert.Empty(t, spoolEntries(t, directory))
}

func TestAuthorizeWindowsRejectsReparseReplacement(t *testing.T) {
	data := []byte("authoritative bytes\n")
	record := inspectCapability(t, data)
	directory := t.TempDir()
	_, err := Authorize(t.Context(), Source{
		Reader: io.NopCloser(bytes.NewReader(data)), Directory: directory,
		testHook: func(stage authorizeStage, path string) error {
			if stage != authorizeStageWritten {
				return nil
			}
			if err := os.Rename(path, path+".original"); err != nil {
				return err
			}
			return os.Symlink(filepath.Base(path)+".original", path)
		},
	}, record, UploadMetadata{Filename: "notes.txt"})
	require.Error(t, err)
	assert.Empty(t, spoolEntries(t, directory))
}

func TestRecoverStaleWindowsRejectsReparseRoot(t *testing.T) {
	target := t.TempDir()
	stale := filepath.Join(target, spoolDirectoryPrefix+"stale")
	require.NoError(t, os.Mkdir(stale, 0o700))
	link := filepath.Join(t.TempDir(), "spool-link")
	require.NoError(t, os.Symlink(target, link))

	_, err := RecoverStale(t.Context(), link)
	require.Error(t, err)
	_, err = os.Stat(stale)
	require.NoError(t, err)
}
