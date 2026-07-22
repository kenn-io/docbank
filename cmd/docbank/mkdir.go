package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

var mkdirJSON bool

var mkdirCmd = &cobra.Command{
	Use:   "mkdir <absolute-virtual-path>",
	Short: "Create a directory in the virtual tree",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateMkdirPathArgument(args[0]); err != nil {
			return err
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		node, err := c.MkdirPath(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if mkdirJSON {
			return writeCLIJSON(cmd.OutOrStdout(), node)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "created %s %s\n",
			formatNodeSelector(node.ID), strconv.Quote(node.Path))
		if err != nil {
			return fmt.Errorf("writing created directory: %w", err)
		}
		return nil
	},
}

func validateMkdirPathArgument(path string) error {
	if !utf8.ValidString(path) {
		return usageError(errors.New("directory path is not valid UTF-8"))
	}
	if !strings.HasPrefix(path, "/") {
		return usageError(errors.New("directory path must be absolute (start with /)"))
	}
	segments := 0
	for segment := range strings.SplitSeq(path, "/") {
		if segment == "" {
			continue
		}
		segments++
		if _, err := store.NormalizeName(segment); err != nil {
			return usageError(fmt.Errorf("directory path %q: %w", path, err))
		}
	}
	if segments == 0 {
		return usageError(errors.New("directory path must not be the vault root"))
	}
	return nil
}

func init() {
	mkdirCmd.Flags().BoolVar(&mkdirJSON, "json", false, "emit machine-readable JSON")
	rootCmd.AddCommand(mkdirCmd)
}
