package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

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
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		c, err := daemonconn.Ensure(ctx)
		if err != nil {
			return err
		}
		err = printChanges(cmd.OutOrStdout(), changesJSON, func(receive func(store.VaultChange) error) error {
			return c.WatchChanges(ctx, changesAfter, changesFollow, receive)
		})
		if changesFollow && ctx.Err() != nil && cmd.Context().Err() == nil && errors.Is(err, context.Canceled) {
			return nil
		}
		return err
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
