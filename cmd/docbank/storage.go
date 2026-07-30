package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

var storageCmd = &cobra.Command{
	Use:   "storage",
	Short: "Inspect and maintain physical blob storage",
}

var (
	storageStatusJSON    bool
	storageStatusRefresh bool
)

var storageStatusCmd = &cobra.Command{
	Use:   "status [store]",
	Short: "Report loose and packed storage usage",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		if len(args) == 1 {
			stores, err := c.BlobStores(cmd.Context(), storageStatusRefresh)
			if err != nil {
				return err
			}
			selected, err := selectBlobStore(stores, args[0])
			if err != nil {
				return err
			}
			if storageStatusJSON {
				return writeStorageJSON(cmd, selected)
			}
			return writeBlobStore(cmd, selected)
		}
		status, err := c.StorageStatus(cmd.Context())
		if err != nil {
			return err
		}
		if storageStatusJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(status)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "loose: %d blob(s), %d byte(s)\n",
			status.LooseBlobs, status.LooseBytes)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"packed: %d live blob(s) in %d pack(s), %d stored byte(s), %d raw byte(s)\n",
			status.PackedBlobs, status.Packs, status.PackedStoredBytes, status.PackedRawBytes)
		if status.DeadPackedBytes > 0 {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "pending repack: %d stored byte(s)\n",
				status.DeadPackedBytes)
		}
		for _, store := range status.Stores {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"store %q (%s): %s, %d object(s), %d stored byte(s)\n",
				store.Name, store.ID, store.State,
				store.AuthoritativeObjects, store.StoredBytes)
		}
		return nil
	},
}

var (
	storageAddBinding  string
	storageAddTakeover bool
	storageAddRun      bool
	storageAddToken    string
	storageAddJSON     bool
)

var storageAddCmd = &cobra.Command{
	Use:   "add [name]",
	Short: "Preview, then attach, one configured secondary store",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if storageAddRun {
			if len(args) != 0 || storageAddBinding != "" || storageAddTakeover {
				return usageError(errors.New(
					"storage add --run uses only the reviewed preview token"))
			}
			if storageAddToken == "" {
				return usageError(errors.New(
					"storage add --run requires --token from a fresh preview"))
			}
			c, err := client.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			result, err := c.RegisterBlobStore(cmd.Context(), storageAddToken)
			if err != nil {
				return err
			}
			if storageAddJSON {
				return writeStorageJSON(cmd, result)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"attached store %q (%s) through binding %q; state: %s\n",
				result.Name, result.ID, result.Binding, result.State)
			return nil
		}
		if storageAddToken != "" {
			return usageError(errors.New("--token requires --run"))
		}
		if len(args) != 1 || storageAddBinding == "" {
			return usageError(errors.New(
				"storage add preview requires a name and --binding"))
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		preview, err := c.PreviewBlobStore(
			cmd.Context(), args[0], storageAddBinding, storageAddTakeover,
		)
		if err != nil {
			return err
		}
		if storageAddJSON {
			return writeStorageJSON(cmd, preview)
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Storage registration preview — no changes made")
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Store: %q (%s)\n", preview.Store.Name, preview.Store.ID)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Binding: %q (%s)\n",
			preview.Store.Binding, preview.Store.Kind)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Ownership marker: %s\n", preview.MarkerAction)
		if preview.Takeover {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(),
				"WARNING: takeover fences the namespace's previous owner.")
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"To attach exactly this store:\n  docbank storage add --run --token %s\n",
			preview.PreviewToken)
		return nil
	},
}

var (
	storageListRefresh bool
	storageListJSON    bool
)

var storageListCmd = &cobra.Command{
	Use:   "list",
	Short: "List primary and secondary physical stores",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		stores, err := c.BlobStores(cmd.Context(), storageListRefresh)
		if err != nil {
			return err
		}
		if storageListJSON {
			return writeStorageJSON(cmd, stores)
		}
		for _, item := range stores {
			if err := writeBlobStore(cmd, item); err != nil {
				return err
			}
		}
		return nil
	},
}

var storageDetachJSON bool

var storageDetachCmd = &cobra.Command{
	Use:   "detach <store>",
	Short: "Detach one empty secondary store",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		item, err := c.DetachBlobStore(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if storageDetachJSON {
			return writeStorageJSON(cmd, item)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "detached store %q (%s)\n", item.Name, item.ID)
		return nil
	},
}

var storageUnregisterCmd = &cobra.Command{
	Use:   "unregister <store>",
	Short: "Forget one detached and empty secondary store",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		if err := c.UnregisterBlobStore(cmd.Context(), args[0]); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "unregistered store %q\n", args[0])
		return nil
	},
}

var (
	storagePackMaxBytes int64
	storagePackJSON     bool
)

var storagePackCmd = &cobra.Command{
	Use:   "pack",
	Short: "Pack authorized loose blobs into immutable pack files",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		report, err := c.StoragePack(cmd.Context(), storagePackMaxBytes)
		if err != nil {
			return err
		}
		if storagePackJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(report)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "packed %d blob(s), %d raw byte(s), sealed %d pack(s)\n",
			report.BlobsPacked, report.BytesPacked, report.PacksSealed)
		if report.LooseSwept > 0 || report.LooseOrphansRemoved > 0 {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"removed %d redundant loose file(s) and %d orphan loose object(s)\n",
				report.LooseSwept, report.LooseOrphansRemoved)
		}
		if report.PacksAdopted+report.PacksRemoved+report.PacksQuarantined+
			report.PacksUnreadable+report.RecordsDropped > 0 || report.MappingsPruned > 0 {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"reconciled packs: %d adopted, %d removed, %d quarantined, %d unreadable; "+
					"%d record(s) dropped, %d mapping(s) pruned\n",
				report.PacksAdopted, report.PacksRemoved, report.PacksQuarantined,
				report.PacksUnreadable, report.RecordsDropped, report.MappingsPruned)
		}
		if report.BlobsMissing+report.BlobsCorrupt+report.BlobsDeferredOversized+
			report.PacksDeferredOversized > 0 {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"deferred: %d missing blob(s), %d corrupt blob(s), %d oversized blob(s), %d oversized pack(s)\n",
				report.BlobsMissing, report.BlobsCorrupt, report.BlobsDeferredOversized,
				report.PacksDeferredOversized)
		}
		if report.LooseOrphanSweepSuppressed {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "orphan loose sweep suppressed: reference inventory was incomplete")
		}
		if report.BudgetExhausted {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "byte budget exhausted; rerun if loose blobs remain")
		}
		return nil
	},
}

var (
	storageRepackMaxBytes     int64
	storageRepackMinAge       time.Duration
	storageRepackMinDeadBytes int64
	storageRepackJSON         bool
)

var storageRepackCmd = &cobra.Command{
	Use:   "repack",
	Short: "Rewrite sparse packs and retire dead pack files",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		report, err := c.StorageRepack(cmd.Context(), storageRepackMaxBytes,
			storageRepackMinAge, storageRepackMinDeadBytes)
		if err != nil {
			return err
		}
		if storageRepackJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(report)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"selected %d pack(s); rewrote %d live blob(s), %d raw byte(s), into %d pack(s)\n",
			report.PacksSelected, report.BlobsRepacked, report.BytesRepacked, report.PacksSealed)
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "retired %d source pack(s); pruned %d stale mapping(s)\n",
			report.PacksRemoved, report.MappingsPruned)
		if report.PacksDeferredOversized > 0 {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "deferred %d oversized pack(s)\n",
				report.PacksDeferredOversized)
		}
		if report.BudgetExhausted {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "byte budget exhausted; rerun to continue selected work")
		}
		return nil
	},
}

func init() {
	storageStatusCmd.Flags().BoolVar(&storageStatusJSON, "json", false, "machine-readable output")
	storageStatusCmd.Flags().BoolVar(&storageStatusRefresh, "refresh", false,
		"perform a fresh ownership-marker check")
	storageAddCmd.Flags().StringVar(&storageAddBinding, "binding", "", "config.toml binding profile")
	storageAddCmd.Flags().BoolVar(&storageAddTakeover, "takeover", false,
		"preview explicit takeover of a namespace owned elsewhere")
	storageAddCmd.Flags().BoolVar(&storageAddRun, "run", false,
		"attach the exact reviewed preview")
	storageAddCmd.Flags().StringVar(&storageAddToken, "token", "", "one-use preview token")
	storageAddCmd.Flags().BoolVar(&storageAddJSON, "json", false, "machine-readable output")
	storageListCmd.Flags().BoolVar(&storageListRefresh, "refresh", false,
		"perform fresh ownership-marker checks")
	storageListCmd.Flags().BoolVar(&storageListJSON, "json", false, "machine-readable output")
	storageDetachCmd.Flags().BoolVar(&storageDetachJSON, "json", false, "machine-readable output")
	storagePackCmd.Flags().Int64Var(&storagePackMaxBytes, "max-bytes", 0,
		"soft raw-byte work budget (0 is unlimited)")
	storagePackCmd.Flags().BoolVar(&storagePackJSON, "json", false, "machine-readable output")
	storageRepackCmd.Flags().Int64Var(&storageRepackMaxBytes, "max-bytes", 0,
		"soft live raw-byte work budget (0 is unlimited and fail-fast)")
	storageRepackCmd.Flags().DurationVar(&storageRepackMinAge, "min-age", 24*time.Hour,
		"minimum source pack age")
	storageRepackCmd.Flags().Int64Var(&storageRepackMinDeadBytes, "min-dead-bytes", 8<<20,
		"minimum dead stored payload in a sparse pack")
	storageRepackCmd.Flags().BoolVar(&storageRepackJSON, "json", false, "machine-readable output")
	storageCmd.AddCommand(
		storageStatusCmd, storageAddCmd, storageListCmd, storageDetachCmd,
		storageUnregisterCmd, storagePackCmd, storageRepackCmd,
	)
	rootCmd.AddCommand(storageCmd)
}

func writeStorageJSON(cmd *cobra.Command, value any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func writeBlobStore(cmd *cobra.Command, item api.BlobStore) error {
	_, err := fmt.Fprintf(cmd.OutOrStdout(),
		"%q %s\n  id: %s\n  kind: %s\n  role: %s\n  lifecycle: %s\n"+
			"  binding: %s\n  state: %s\n  authority: %d object(s), %d logical byte(s), %d stored byte(s)\n",
		item.Name, item.State, item.ID, item.Kind, item.Role, item.Lifecycle,
		item.Binding, item.State, item.AuthoritativeObjects,
		item.LogicalBytes, item.StoredBytes)
	if err != nil {
		return fmt.Errorf("writing blob-store status: %w", err)
	}
	if item.Detail != "" {
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "  detail: %s\n", item.Detail)
	}
	if err != nil {
		return fmt.Errorf("writing blob-store detail: %w", err)
	}
	return nil
}

func selectBlobStore(stores []api.BlobStore, selector string) (api.BlobStore, error) {
	idSelector := client.IsCanonicalUUIDv4(selector)
	for _, item := range stores {
		if (idSelector && item.ID == selector) || (!idSelector && item.Name == selector) {
			return item, nil
		}
	}
	return api.BlobStore{}, fmt.Errorf("blob store %q: not found", selector)
}
