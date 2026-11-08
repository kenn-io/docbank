package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/brainclient"
	"go.kenn.io/docbank/internal/home"
)

func newBrainCmd() *cobra.Command {
	var configRoot string
	cmd := &cobra.Command{Use: "brain", Short: "Connect and publish a Markdown folder", GroupID: groupSources}
	cmd.PersistentFlags().StringVar(&configRoot, "config-root", "", "client configuration directory")
	registry := func() (*brainclient.Registry, error) {
		root := configRoot
		if root == "" {
			var err error
			root, err = brainclient.DefaultConfigRoot()
			if err != nil {
				return nil, err
			}
		}
		return brainclient.OpenRegistry(root)
	}
	var name, destination, vaultRoot, remote, keyFile string
	var excludes []string
	var indexLocal bool
	var settle, age, interval time.Duration
	connect := &cobra.Command{Use: "connect <local-dir>", Short: "Save a folder and its daemon target", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if vaultRoot != "" && remote != "" {
			return errors.New("choose either --vault-root or --to")
		}
		if remote == "" && keyFile != "" {
			return errors.New("--api-key-file requires --to")
		}
		source, err := home.CanonicalRoot(args[0])
		if err != nil {
			return err
		}
		if vaultRoot == "" && remote == "" {
			layout, err := home.Resolve()
			if err != nil {
				return err
			}
			vaultRoot = layout.Root
		} else if vaultRoot != "" {
			vaultRoot, err = home.CanonicalRoot(vaultRoot)
			if err != nil {
				return err
			}
		}
		if keyFile != "" {
			keyFile, err = filepath.Abs(keyFile)
			if err != nil {
				return err
			}
		}
		r, err := registry()
		if err != nil {
			return err
		}
		cfg := brainclient.ConnectionConfig{Name: name, SourceRoot: source, Destination: destination, LocalVaultRoot: vaultRoot,
			RemoteOrigin: remote, KeyFile: keyFile, Exclude: excludes, SettleTime: settle, MinimumAge: age, ScanInterval: interval, IndexLocal: indexLocal}
		if err := brainclient.New(r, brainclient.Dependencies{}).Connect(cmd.Context(), cfg); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "Connection saved. Run it with docbank brain run", name)
		return err
	}}
	connect.Flags().StringVar(&name, "name", "", "stable connection name")
	connect.Flags().StringVar(&destination, "dest", "", "absolute destination in the vault")
	connect.Flags().StringVar(&vaultRoot, "vault-root", "", "local daemon vault root")
	connect.Flags().StringVar(&remote, "to", "", "remote daemon URL")
	connect.Flags().StringVar(&keyFile, "api-key-file", "", "existing remote API key file")
	connect.Flags().StringSliceVar(&excludes, "exclude", nil, "exclude a relative path")
	connect.Flags().BoolVar(&indexLocal, "index-local", false, "authorize local text indexing")
	connect.Flags().DurationVar(&settle, "settle-time", 2*time.Second, "settle time")
	connect.Flags().DurationVar(&age, "minimum-age", 0, "minimum file age")
	connect.Flags().DurationVar(&interval, "scan-interval", 5*time.Second, "scan interval")
	_ = connect.MarkFlagRequired("name")
	_ = connect.MarkFlagRequired("dest")
	run := &cobra.Command{Use: "run <name>", Short: "Publish changes until stopped", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		r, err := registry()
		if err != nil {
			return err
		}
		report, err := brainclient.New(r, brainclient.Dependencies{}).Run(cmd.Context(), args[0], nil)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Added %d, updated %d, unchanged %d, failed %d\n", report.Added, report.Updated, report.Skipped, report.Failed)
		return err
	}}
	status := &cobra.Command{Use: "status <name>", Short: "Inspect a saved connection", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		r, err := registry()
		if err != nil {
			return err
		}
		result, err := brainclient.New(r, brainclient.Dependencies{}).Status(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return json.MarshalWrite(cmd.OutOrStdout(), result)
	}}
	cmd.AddCommand(connect, run, status)
	return cmd
}

func init() { rootCmd.AddCommand(newBrainCmd()) }
