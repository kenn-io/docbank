package main

import (
	"errors"
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

func TestMediaOperationCLIRequiresOneUUID(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	_, err := runCLI(t, "media", "operation")
	require.ErrorContains(t, err, "accepts 1 arg(s)")
	// Argument validation fails before RunE starts, so main maps it to a usage exit.
	require.Equal(t, exitUsage, commandExitCode(err, false))

	_, err = runCLI(t, "media", "operation", "not-a-uuid")
	require.ErrorContains(t, err, "operation ID must be a UUID")
	require.Equal(t, exitUsage, commandExitCode(err, true))
}

func TestMediaTranscriptCLIRequiresStableVersionFlags(t *testing.T) {
	t.Setenv("DOCBANK_HOME", t.TempDir())
	for _, flags := range [][]string{
		{"--source-version-id", "version"},
		{"--content-version-id", "00000000-0000-4000-8000-000000000001"},
	} {
		_, err := runCLI(t, append([]string{"media", "transcript", "source"}, flags...)...)
		exit, ok := errors.AsType[*exitError](err)
		require.True(t, ok, "missing version flag must be a usage error: %v", err)
		require.Equal(t, exitUsage, exit.code)
	}
}
