package cmd

import (
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

var trashCmd = &cobra.Command{
	Use:   "trash",
	Short: "Inspect and empty the trash",
}

var trashListCmd = &cobra.Command{
	Use:   "list",
	Short: "List restorable trashed nodes",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		v, err := openVault()
		if err != nil {
			return err
		}
		defer func() { _ = v.close() }()

		roots, err := v.store.TrashedRoots(cmd.Context())
		if err != nil {
			return err
		}
		if len(roots) == 0 {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "trash is empty")
			return nil
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "ID\tTRASHED AT\tNAME")
		for _, n := range roots {
			trashedAt := ""
			if n.TrashedAt != nil {
				trashedAt = *n.TrashedAt
			}
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\n", n.ID, trashedAt, n.Name)
		}
		return w.Flush()
	},
}

var trashOlderThan string

var trashEmptyCmd = &cobra.Command{
	Use:   "empty",
	Short: "Permanently delete trashed nodes (their blobs become gc candidates)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		age, err := parseAge(trashOlderThan)
		trashOlderThan = "" // package-level flag vars persist across in-process Execute calls
		if err != nil {
			return err
		}
		v, err := openVault()
		if err != nil {
			return err
		}
		defer func() { _ = v.close() }()

		n, err := v.store.EmptyTrash(cmd.Context(), age)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "deleted %d trashed node(s)\n", n)
		return nil
	},
}

// parseAge parses Go durations plus a day suffix: "30d" = 30*24h. Empty
// means zero (everything). Negative ages are rejected: a future cutoff
// would silently delete the entire trash.
func parseAge(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	var d time.Duration
	if base, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(base)
		if err != nil {
			return 0, fmt.Errorf("invalid age %q (want e.g. 30d or 12h): %w", s, err)
		}
		d = time.Duration(days) * 24 * time.Hour
	} else {
		var err error
		d, err = time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("invalid age %q (want e.g. 30d or 12h): %w", s, err)
		}
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid age %q: must not be negative", s)
	}
	return d, nil
}

func init() {
	trashEmptyCmd.Flags().StringVar(&trashOlderThan, "older-than", "",
		"only delete items trashed at least this long ago (e.g. 30d)")
	trashCmd.AddCommand(trashListCmd, trashEmptyCmd)
	rootCmd.AddCommand(trashCmd)
}
