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
	"golang.org/x/term"
)

func readPhotoPasscode(cmd *cobra.Command, input *bufio.Reader, prompt string) (string, error) {
	if file, ok := cmd.InOrStdin().(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		_, _ = fmt.Fprint(cmd.ErrOrStderr(), prompt+": ")
		bytes, err := term.ReadPassword(int(file.Fd()))
		_, _ = fmt.Fprintln(cmd.ErrOrStderr())
		if err != nil {
			return "", err
		}
		if len(bytes) < 1 || len(bytes) > 1024 {
			return "", errors.New("passcode must be 1 to 1024 bytes")
		}
		return string(bytes), nil
	}
	bytes, err := input.ReadSlice('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(bytes), "\n"), "\r")
	if len(value) < 1 || len(value) > 1024 {
		return "", errors.New("passcode must be 1 to 1024 bytes")
	}
	return value, nil
}

func init() {
	hidden := &cobra.Command{Use: "hidden", Short: "Configure Hidden photos access", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	photosCmd.AddCommand(hidden)
	for _, action := range []string{"setup", "change", "disable", "lock", "reset", "state"} {
		command := &cobra.Command{Use: action, Short: action + " Hidden photos access", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			if action == "state" {
				state, err := connection.PhotoHiddenState(cmd.Context())
				if err != nil {
					return err
				}
				return writeCLIJSON(cmd.OutOrStdout(), state)
			}
			var passcode, next string
			input := bufio.NewReaderSize(cmd.InOrStdin(), 2048)
			if action != "lock" && action != "reset" {
				passcode, err = readPhotoPasscode(cmd, input, "Passcode")
				if err != nil {
					return err
				}
			}
			if action == "change" {
				next, err = readPhotoPasscode(cmd, input, "New passcode")
				if err != nil {
					return err
				}
			}
			state, _, err := connection.PhotoHidden(cmd.Context(), action, passcode, next)
			if err != nil {
				return err
			}
			return writeCLIJSON(cmd.OutOrStdout(), state)
		}}
		hidden.AddCommand(command)
	}
	for _, hide := range []bool{true, false} {
		action := "hide"
		if !hide {
			action = "unhide"
		}
		command := &cobra.Command{Use: action + " <asset-id>", Short: action + " a photo in Photos", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkPhotoRevisionFlag(cmd); err != nil {
				return err
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			var cookie string
			if !hide {
				passcode, err := readPhotoPasscode(cmd, bufio.NewReaderSize(cmd.InOrStdin(), 2048), "Passcode")
				if err != nil {
					return err
				}
				_, cookie, err = connection.PhotoHidden(cmd.Context(), "unlock", passcode, "")
				if err != nil {
					return err
				}
			}
			asset, err := withPhotoRevision(cmd, func() (*int64, error) {
				asset, err := connection.PhotoAsset(cmd.Context(), args[0], cookie)
				if err != nil {
					return nil, err
				}
				return &asset.Revision, nil
			}, func(revision *int64) (api.PhotoAsset, error) {
				return connection.SetPhotoAssetHidden(cmd.Context(), args[0], *revision, hide, cookie)
			})
			if err != nil {
				return err
			}
			return writeCLIJSON(cmd.OutOrStdout(), asset)
		}}
		command.Flags().Int64Var(&photoRevision, "revision", 0, "Expected asset revision")
		photosCmd.AddCommand(command)
	}
}
