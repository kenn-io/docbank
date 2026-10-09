package main

import (
	"bufio"
	"encoding/json/v2"
	"image/color"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
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
}

func TestPhotoHiddenCLIUnhideBySelector(t *testing.T) {
	_ = setupVaultHome(t)
	source := writeSourceFile(t, "synthetic-hidden.jpeg", string(mediatest.JPEG(2, 2, color.White)))
	_, err := runCLI(t, "add", source, "--dest", "/inbox")
	require.NoError(t, err)
	out, err := runCLI(t, "photos", "assets", "inspect", "/inbox/synthetic-hidden.jpeg")
	require.NoError(t, err)
	var asset api.PhotoAsset
	require.NoError(t, json.Unmarshal([]byte(out), &asset))
	_, err = runCLI(t, "photos", "hide", asset.ID)
	require.ErrorIs(t, err, store.ErrHiddenNotConfigured)
	require.EqualError(t, err, "error executing request: configure a hidden photos passcode first")
	connection, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	_, _, err = connection.PhotoHidden(t.Context(), "setup", "synthetic-passcode", "")
	require.NoError(t, err)
	unhide, _, err := rootCmd.Find([]string{"photos", "unhide"})
	require.NoError(t, err)
	t.Cleanup(func() { unhide.SetIn(nil) })
	for _, selector := range []string{"/inbox/synthetic-hidden.jpeg", "id:" + strconv.FormatInt(asset.Files[0].NodeID, 10), asset.ID} {
		_, err = runCLI(t, "photos", "hide", asset.ID)
		require.NoError(t, err)
		_, err = runCLI(t, "photos", "assets", "inspect", "/inbox/synthetic-hidden.jpeg")
		require.ErrorIs(t, err, store.ErrHiddenLocked)
		require.EqualError(t, err, "error executing request: hidden photos are locked; unlock Hidden before retrying")
		unhide.SetIn(strings.NewReader("incorrect-passcode\n"))
		_, err = runCLI(t, "photos", "unhide", selector)
		require.ErrorIs(t, err, store.ErrHiddenPasscode)
		require.EqualError(t, err, "error executing request: incorrect hidden photos passcode; retry with the correct passcode")
		unhide.SetIn(strings.NewReader("synthetic-passcode\n"))
		out, err = runCLI(t, "photos", "unhide", selector)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal([]byte(out), &asset))
		require.Nil(t, asset.HiddenAt)
		_, err = runCLI(t, "photos", "assets", "inspect", "/inbox/synthetic-hidden.jpeg")
		require.NoError(t, err)
	}
}
