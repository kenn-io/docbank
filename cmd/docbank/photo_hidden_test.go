package main

import (
	"bufio"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestPhotoHiddenPasscodeInput(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(" current passcode \r\nnew passcode\n"))
	input := bufio.NewReaderSize(cmd.InOrStdin(), 2048)
	first, err := readPhotoPasscode(cmd, input, "Current")
	require.NoError(t, err)
	require.Equal(t, " current passcode ", first)
	next, err := readPhotoPasscode(cmd, input, "New")
	require.NoError(t, err)
	require.Equal(t, "new passcode", next)
	for _, value := range []string{"\n", strings.Repeat("x", 1025) + "\n", strings.Repeat("x", 4096)} {
		cmd.SetIn(strings.NewReader(value))
		_, err := readPhotoPasscode(cmd, bufio.NewReaderSize(cmd.InOrStdin(), 2048), "Passcode")
		require.Error(t, err)
	}
}
