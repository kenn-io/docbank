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
