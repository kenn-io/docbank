package main

import (
	"bufio"
	"encoding/json/v2"
	"image/color"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media/mediatest"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/config"
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

func TestPhotoHiddenNewPasscodeConfirmation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, input, want, err string
		confirm                bool
	}{
		{name: "piped input reads one line", input: "first\nsecond\n", want: "first"},
		{name: "matching confirmation", input: "same\nsame\n", want: "same", confirm: true},
		{name: "mismatched confirmation", input: "typed\ntypo\n", err: "passcodes do not match", confirm: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := &cobra.Command{}
			cmd.SetIn(strings.NewReader(tc.input))
			got, err := readNewPhotoPasscode(cmd, bufio.NewReaderSize(cmd.InOrStdin(), 2048), "Passcode", tc.confirm)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestPhotoHiddenInvocationUnlock(t *testing.T) {
	for _, passcode := range []string{"synthetic-passcode", "wrong-passcode", "", "\n"} {
		t.Run(passcode, func(t *testing.T) {
			dir := t.TempDir()
			s, err := store.Open(filepath.Join(dir, "docbank.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			ctx := t.Context()
			node, err := s.CreateFile(ctx, s.RootID(), "synthetic.jpg", strings.Repeat("a", 64), 20, "image/jpeg")
			require.NoError(t, err)
			asset, err := s.PhotoAssetForNode(ctx, node.ID)
			require.NoError(t, err)
			require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
			asset, err = s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
			require.NoError(t, err)
			cfg := config.Default()
			cfg.Server.APIKey = "synthetic-key"
			server := api.NewServer(api.Deps{Store: s, VaultRoot: dir, Cfg: cfg})
			t.Cleanup(server.Close)
			unlocks, writes := 0, 0
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/unlock") {
					unlocks++
				}
				if strings.HasSuffix(r.URL.Path, "/exclude") {
					writes++
				}
				server.Handler().ServeHTTP(w, r)
			}))
			t.Cleanup(ts.Close)
			connection := daemonconn.New(ts.URL, cfg.Server.APIKey)
			cmd := &cobra.Command{}
			cmd.SetContext(ctx)
			input := passcode
			if passcode != "" && passcode != "\n" {
				input += "\n"
			}
			cmd.SetIn(strings.NewReader(input))
			result, err := withPhotoHidden(cmd, connection, func(cookie string) (api.PhotoAsset, error) {
				return connection.ExcludePhotoAsset(ctx, asset.ID, asset.Revision, true, cookie)
			})
			if passcode == "" || passcode == "\n" {
				require.ErrorIs(t, err, store.ErrHiddenLocked)
				require.EqualError(t, err, "hidden photos are locked: no passcode supplied")
				require.Zero(t, unlocks)
				require.Equal(t, 1, writes)
				return
			}
			require.Equal(t, 1, unlocks, "operation error: %v", err)
			if passcode == "wrong-passcode" {
				require.ErrorIs(t, err, store.ErrHiddenPasscode)
				require.EqualError(t, err, "error executing request: incorrect hidden photos passcode; retry with the correct passcode")
				require.Equal(t, 1, writes)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 2, writes)
			require.NotNil(t, result.HiddenAt)
			require.NotNil(t, result.ExcludedAt)
			_, err = connection.PhotoAsset(ctx, asset.ID)
			require.ErrorIs(t, err, store.ErrHiddenLocked)
		})
	}
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
	_, err = runCLI(t, "photos", "assets", "hide", asset.ID)
	require.ErrorIs(t, err, store.ErrHiddenNotConfigured)
	require.EqualError(t, err, "error executing request: configure a hidden photos passcode first")
	connection, err := daemonconn.Ensure(t.Context())
	require.NoError(t, err)
	_, _, err = connection.PhotoHidden(t.Context(), "setup", "synthetic-passcode", "")
	require.NoError(t, err)
	unhide, _, err := rootCmd.Find([]string{"photos", "assets", "unhide"})
	require.NoError(t, err)
	t.Cleanup(func() { unhide.SetIn(nil); photoInspectCmd.SetIn(nil) })
	_, err = runCLI(t, "photos", "assets", "hide", asset.ID)
	require.NoError(t, err)
	photoInspectCmd.SetIn(strings.NewReader("synthetic-passcode\n"))
	out, err = runCLI(t, "photos", "assets", "inspect", "/inbox/synthetic-hidden.jpeg")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(out), &asset))
	require.NotNil(t, asset.HiddenAt)
	for i, selector := range []string{"/inbox/synthetic-hidden.jpeg", "id:" + strconv.FormatInt(asset.Files[0].NodeID, 10), asset.ID} {
		if i > 0 {
			_, err = connection.SetPhotoAssetHidden(t.Context(), asset.ID, asset.Revision, true, "")
			require.NoError(t, err)
		}
		unhide.SetIn(strings.NewReader("synthetic-passcode\n"))
		out, err = runCLI(t, "photos", "assets", "unhide", selector)
		require.NoError(t, err)
		asset = api.PhotoAsset{}
		require.NoError(t, json.Unmarshal([]byte(out), &asset))
		require.Nil(t, asset.HiddenAt)
		_, err = runCLI(t, "photos", "assets", "inspect", "/inbox/synthetic-hidden.jpeg")
		require.NoError(t, err)
	}
}

func TestPhotoHiddenCLIJSONContract(t *testing.T) {
	_ = setupVaultHome(t)
	for _, tc := range []struct{ action, input string }{
		{"setup", "synthetic-passcode\n"},
		{"state", ""},
	} {
		t.Run(tc.action, func(t *testing.T) {
			cmd, _, err := rootCmd.Find([]string{"photos", "hidden", tc.action})
			require.NoError(t, err)
			cmd.SetIn(strings.NewReader(tc.input))
			t.Cleanup(func() { cmd.SetIn(nil) })
			out, err := runCLI(t, "photos", "hidden", tc.action)
			require.NoError(t, err)
			var result map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &result))
			require.Contains(t, result, "configured")
			require.NotContains(t, result, "change_cursor")
		})
	}
}
