package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestMediaCLIKeepsPrivateReferencesOutOfArguments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reference")
	require.NoError(t, os.WriteFile(path, []byte("https://recordings.invalid/private?token=secret\n"), 0o600))
	value, err := readPrivateReference(path)
	require.NoError(t, err)
	require.Contains(t, value, "secret")
	for _, command := range []*cobra.Command{mediaSubmitCmd, mediaAcquisitionPlanCmd} {
		require.Nil(t, command.Flags().Lookup("url"))
		require.NotNil(t, command.Flags().Lookup("reference-file"))
	}
	require.Empty(t, mediaSubmitFilename(""))
	require.Equal(t, "recording.wav", mediaSubmitFilename(filepath.Join("private", "recording.wav")))
}

func TestOpenMediaUploadUsesFixedMediaTypes(t *testing.T) {
	for extension, want := range map[string]string{
		".mp4": "video/mp4", ".srt": "application/x-subrip", ".wav": "audio/wav",
		".mp3": "audio/mpeg", ".txt": "text/plain; charset=utf-8",
	} {
		t.Run(extension, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "upload"+extension)
			require.NoError(t, os.WriteFile(path, []byte("synthetic"), 0o600))
			file, metadata, err := openMediaUpload(path)
			require.NoError(t, err)
			require.NoError(t, file.Close())
			require.Equal(t, want, metadata.MediaType)
		})
	}
}
