package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

var changesAfter string
var changesFollow, changesJSON bool
var changesCmd = &cobra.Command{
	GroupID: groupInterfaces,
	Example: `  docbank changes --follow --json`,
	Use:     "changes", Short: "Read resumable vault changes", Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		return printChanges(cmd.OutOrStdout(), changesJSON, func(receive func(store.VaultChange) error) error {
			return c.WatchChanges(cmd.Context(), changesAfter, changesFollow, receive)
		})
	},
}

func printChanges(w io.Writer, asJSON bool, watch func(func(store.VaultChange) error) error) error {
	encoder := jsontext.NewEncoder(w)
	return watch(func(change store.VaultChange) error {
		if asJSON {
			return json.MarshalEncode(encoder, change)
		}
		_, err := fmt.Fprintf(w, "%s\t%s\t%d\t%d\n", change.Cursor, change.Kind, change.NodeID, change.Revision)
		if err != nil {
			return fmt.Errorf("writing vault change: %w", err)
		}
		return nil
	})
}

func init() {
	changesCmd.Flags().StringVar(&changesAfter, "after", "", "resume after this cursor")
	changesCmd.Flags().BoolVar(&changesFollow, "follow", false, "wait for new changes")
	changesCmd.Flags().BoolVar(&changesJSON, "json", false, "print JSON, one event per line")
	rootCmd.AddCommand(changesCmd)
}
