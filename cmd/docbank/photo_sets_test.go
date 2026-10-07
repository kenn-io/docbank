package main

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPhotoAlbumCLICommands(t *testing.T) {
	for _, name := range []string{"list", "show", "create", "rename", "star", "cover", "duplicate", "delete", "add", "remove", "members"} {
		command, _, err := rootCmd.Find([]string{"photos", "albums", name})
		require.NoError(t, err)
		require.Equal(t, name, command.Name())
		if name == "add" || name == "remove" {
			require.NotNil(t, command.Flags().Lookup("query"))
			require.NotNil(t, command.Flags().Lookup("revision"))
		}
	}
}
