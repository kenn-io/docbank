package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
	"golang.org/x/term"
)

func photoPasscodeTerminal(cmd *cobra.Command) (*os.File, bool) {
	file, ok := cmd.InOrStdin().(*os.File)
	return file, ok && term.IsTerminal(int(file.Fd()))
}

func readPhotoPasscode(cmd *cobra.Command, input *bufio.Reader, prompt string) (string, error) {
	if file, ok := photoPasscodeTerminal(cmd); ok {
		_, _ = fmt.Fprint(cmd.ErrOrStderr(), prompt+": ")
		bytes, err := term.ReadPassword(int(file.Fd()))
		_, _ = fmt.Fprintln(cmd.ErrOrStderr())
		if err != nil {
			return "", fmt.Errorf("reading hidden passcode: %w", err)
		}
		return string(bytes), nil
	}
	bytes, err := input.ReadSlice('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading hidden passcode: %w", err)
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(bytes), "\n"), "\r")
	return value, nil
}

// readNewPhotoPasscode asks a terminal user to repeat a new passcode; piped input stays one line.
func readNewPhotoPasscode(cmd *cobra.Command, input *bufio.Reader, prompt string, confirm bool) (string, error) {
	passcode, err := readPhotoPasscode(cmd, input, prompt)
	if err != nil || !confirm {
		return passcode, err
	}
	confirmation, err := readPhotoPasscode(cmd, input, "Confirm "+strings.ToLower(prompt[:1])+prompt[1:])
	if err != nil {
		return "", err
	}
	if confirmation != passcode {
		return "", errors.New("passcodes do not match; nothing changed")
	}
	return passcode, nil
}

func withPhotoHidden[T any](cmd *cobra.Command, connection *daemonconn.Connection, run func(string) (T, error)) (T, error) {
	result, err := run("")
	if !errors.Is(err, store.ErrHiddenLocked) {
		return result, err
	}
	passcode, err := readPhotoPasscode(cmd, bufio.NewReaderSize(cmd.InOrStdin(), 2048), "Passcode")
	if err != nil {
		return result, err
	}
	if passcode == "" {
		return result, fmt.Errorf("%w: no passcode supplied", store.ErrHiddenLocked)
	}
	_, cookie, err := connection.PhotoHidden(cmd.Context(), "unlock", passcode, "")
	if err != nil {
		return result, err
	}
	return run(cookie)
}

func peopleHiddenCookie(cmd *cobra.Command, c *daemonconn.Connection) (string, error) {
	unlock, err := cmd.Flags().GetBool("unlock-hidden")
	if err != nil {
		return "", fmt.Errorf("reading --unlock-hidden: %w", err)
	}
	if !unlock {
		return "", nil
	}
	passcode, err := readPhotoPasscode(cmd, bufio.NewReaderSize(cmd.InOrStdin(), 2048), "Passcode")
	if err != nil {
		return "", err
	}
	_, cookie, err := c.PhotoHidden(cmd.Context(), "unlock", passcode, "")
	return cookie, err
}

func init() {
	hidden := &cobra.Command{Use: "hidden", Short: "Configure Hidden photos access", Long: "setup sets a passcode; change and disable require the current passcode.\nlock revokes sessions; reset removes the passcode and keeps photos hidden.\nAfter reset, run docbank photos hidden setup to set a replacement passcode.\nstate reports whether Hidden is configured.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	photosCmd.AddCommand(hidden)
	for _, action := range []string{"setup", "change", "disable", "lock", "reset", "state"} {
		command := &cobra.Command{Use: action, Short: action + " Hidden photos access", Example: "  docbank photos hidden " + action, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			if action == "state" {
				state, err := connection.PhotoHiddenState(cmd.Context())
				if err != nil {
					return err
				}
				state.ChangeCursor = ""
				return writeCLIJSON(cmd.OutOrStdout(), state)
			}
			var passcode, next string
			input := bufio.NewReaderSize(cmd.InOrStdin(), 2048)
			_, terminal := photoPasscodeTerminal(cmd)
			if action == "setup" {
				passcode, err = readNewPhotoPasscode(cmd, input, "Passcode", terminal)
			} else if action != "lock" && action != "reset" {
				passcode, err = readPhotoPasscode(cmd, input, "Passcode")
			}
			if err != nil {
				return err
			}
			if action == "change" {
				next, err = readNewPhotoPasscode(cmd, input, "New passcode", terminal)
				if err != nil {
					return err
				}
			}
			state, _, err := connection.PhotoHidden(cmd.Context(), action, passcode, next)
			if err != nil {
				return err
			}
			state.ChangeCursor = ""
			return writeCLIJSON(cmd.OutOrStdout(), state)
		}}
		hidden.AddCommand(command)
	}
	for _, hide := range []bool{true, false} {
		action := "hide"
		if !hide {
			action = "unhide"
		}
		command := &cobra.Command{Use: action + " <asset-id>", Short: action + " a photo in Photos", Example: "  docbank photos assets " + action + " <asset-id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkPhotoRevisionFlag(cmd); err != nil {
				return err
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			asset, err := withPhotoHidden(cmd, connection, func(cookie string) (api.PhotoAsset, error) {
				assetID := args[0]
				if !hide && (strings.HasPrefix(assetID, nodeIDSelectorPrefix) || strings.HasPrefix(assetID, "/")) {
					selector, err := parseNodeSelector(assetID)
					if err != nil {
						return api.PhotoAsset{}, err
					}
					node, err := selector.resolveIncludingTrash(cmd.Context(), connection)
					if err != nil {
						return api.PhotoAsset{}, err
					}
					asset, err := connection.PhotoAssetForNode(cmd.Context(), node.ID, cookie)
					if err != nil {
						return api.PhotoAsset{}, err
					}
					assetID = asset.ID
				}
				return withPhotoRevision(cmd, func() (*int64, error) {
					asset, err := connection.PhotoAsset(cmd.Context(), assetID, cookie)
					if err != nil {
						return nil, err
					}
					return &asset.Revision, nil
				}, func(revision *int64) (api.PhotoAsset, error) {
					return connection.SetPhotoAssetHidden(cmd.Context(), assetID, *revision, hide, cookie)
				})
			})
			if err != nil {
				return err
			}
			return writeCLIJSON(cmd.OutOrStdout(), asset)
		}}
		if !hide {
			command.Use = action + " <asset-id|path-or-id>"
		} else {
			command.Short = "Hide a photo by asset ID"
		}
		command.Flags().Int64Var(&photoRevision, "revision", 0, "expected asset revision (default: current, retried once)")
		photoAssetsCmd.AddCommand(command)
	}
}
