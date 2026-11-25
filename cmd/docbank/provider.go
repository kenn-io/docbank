package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/docbank/provider"
	"go.kenn.io/docbank/internal/providers"
)

var (
	providerConfigureJSON bool
	providerListJSON      bool
	providerVaultsJSON    bool
)

var providerCmd = &cobra.Command{
	Example: `  docbank provider list
  docbank provider vaults work --json`,
	Use:   "provider",
	Short: "Connect to vaults hosted by another service",
	Long: `A provider is a service that hosts Docbank vaults for you. Docbank asks the
provider's helper, a trusted program on this computer, which vaults you can
open. Connections are saved in providers.toml in your user configuration
directory, or in $DOCBANK_CONFIG_HOME when it is set.`,
	GroupID:     groupInterfaces,
	Annotations: map[string]string{helpOnlyAnnotation: helpOnlyMarker},
	RunE:        commandGroupHelp,
}

// providerConfiguration is the document provider configure reads from stdin.
type providerConfiguration struct {
	Version       int      `json:"version"`
	Name          string   `json:"name"`
	Origin        string   `json:"origin"`
	MountPrefix   string   `json:"mount_prefix"`
	BrowserOrigin string   `json:"browser_origin"`
	Helper        []string `json:"helper"`
}

var providerConfigureCmd = &cobra.Command{
	Example: `  docbank provider configure --json`,
	Use:     "configure",
	Short:   "Add or replace a provider connection from a JSON document on stdin",
	Long: `Reads one JSON document from stdin:

  {"version": 1, "name": "work",
   "origin": "https://vaults.example.org", "mount_prefix": "/documents",
   "browser_origin": "https://vaults.example.org",
   "helper": ["/usr/local/bin/example-helper", "--connection", "work"]}

Docbank asks the helper which vaults you can open and saves the connection
only when the helper answers. A failed check leaves the saved connections
unchanged. Other providers and this installation's ID are kept.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		var input providerConfiguration
		data, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), provider.MaxRequestBytes+1))
		if err != nil {
			return fmt.Errorf("reading the provider configuration: %w", err)
		}
		if err := json.Unmarshal(data, &input, json.RejectUnknownMembers(true), jsontext.AllowDuplicateNames(false)); err != nil {
			return usageError(fmt.Errorf("reading the provider configuration: %w", err))
		}
		if input.Version != 1 {
			return usageError(errors.New("the provider configuration must have version 1"))
		}
		config := providers.Provider{
			Origin: input.Origin, MountPrefix: input.MountPrefix,
			BrowserOrigin: input.BrowserOrigin, Helper: input.Helper,
		}
		if err := providers.Validate(input.Name, config); err != nil {
			return usageError(err)
		}
		store, err := providers.DefaultStore()
		if err != nil {
			return err
		}
		var discovery provider.Discovery
		err = store.Update(func(file *providers.File) error {
			discovery, err = discoverVaults(cmd.Context(), input.Name, providers.Connection(*file, config), config.Helper)
			if err != nil {
				return err
			}
			file.Providers[input.Name] = config
			return nil
		})
		if err != nil {
			return err
		}
		if providerConfigureJSON {
			return writeCLIJSON(cmd.OutOrStdout(), discovery)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Connected provider %s: %d vault(s)\n", input.Name, len(discovery.Vaults))
		return nil
	},
}

var providerListCmd = &cobra.Command{
	Example: `  docbank provider list --json`,
	Use:     "list",
	Short:   "List provider connections",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		file, err := loadProviders()
		if err != nil {
			return err
		}
		type listed struct {
			Name          string   `json:"name"`
			Origin        string   `json:"origin"`
			MountPrefix   string   `json:"mount_prefix"`
			BrowserOrigin string   `json:"browser_origin"`
			Helper        []string `json:"helper"`
		}
		rows := make([]listed, 0, len(file.Providers))
		for _, name := range slices.Sorted(maps.Keys(file.Providers)) {
			p := file.Providers[name]
			rows = append(rows, listed{Name: name, Origin: p.Origin, MountPrefix: p.MountPrefix, BrowserOrigin: p.BrowserOrigin, Helper: p.Helper})
		}
		if providerListJSON {
			return writeCLIJSON(cmd.OutOrStdout(), rows)
		}
		if len(rows) == 0 {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No provider connections")
		}
		for _, row := range rows {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s%s\n", row.Name, row.Origin, row.MountPrefix)
		}
		return nil
	},
}

var providerVaultsCmd = &cobra.Command{
	Example: `  docbank provider vaults work --json`,
	Use:     "vaults <name>",
	Short:   "List the vaults a provider lets you open",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		file, err := loadProviders()
		if err != nil {
			return err
		}
		config, ok := file.Providers[args[0]]
		if !ok {
			return usageError(fmt.Errorf("no provider named %q; run docbank provider list", args[0]))
		}
		discovery, err := discoverVaults(cmd.Context(), args[0], providers.Connection(file, config), config.Helper)
		if err != nil {
			return err
		}
		if providerVaultsJSON {
			return writeCLIJSON(cmd.OutOrStdout(), discovery)
		}
		out := cmd.OutOrStdout()
		switch {
		case !discovery.Enabled:
			_, _ = fmt.Fprintf(out, "Provider %s does not offer vaults\n", args[0])
		case len(discovery.Vaults) == 0:
			_, _ = fmt.Fprintln(out, "No vaults")
		}
		for _, vault := range discovery.Vaults {
			_, _ = fmt.Fprintf(out, "%s\t%s\n", vault.ID, vault.Name)
		}
		return nil
	},
}

func loadProviders() (providers.File, error) {
	store, err := providers.DefaultStore()
	if err != nil {
		return providers.File{}, err
	}
	return store.Load()
}

func discoverVaults(ctx context.Context, name string, connection provider.Connection, helper []string) (provider.Discovery, error) {
	request, err := provider.NewDiscoverRequest(connection)
	if err != nil {
		return provider.Discovery{}, err
	}
	response, err := provider.Exchange(ctx, helper, request)
	if err != nil {
		return provider.Discovery{}, fmt.Errorf("asking provider %s for vaults: %w", name, err)
	}
	switch response.Status {
	case provider.StatusReady:
		return *response.Discovery, nil
	case provider.StatusSignInRequired:
		return provider.Discovery{}, fmt.Errorf("provider %s needs you to sign in to its service; sign in there, then retry", name)
	case provider.StatusDenied:
		return provider.Discovery{}, fmt.Errorf("provider %s denied access%s", name, reasonSuffix(response.Reason))
	default:
		return provider.Discovery{}, fmt.Errorf("provider %s is unavailable%s; retry later", name, reasonSuffix(response.Reason))
	}
}

func reasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return " (" + reason + ")"
}

func init() {
	providerConfigureCmd.Flags().BoolVar(&providerConfigureJSON, "json", false, "print JSON to stdout")
	providerListCmd.Flags().BoolVar(&providerListJSON, "json", false, "print JSON to stdout")
	providerVaultsCmd.Flags().BoolVar(&providerVaultsJSON, "json", false, "print JSON to stdout")
	providerCmd.AddCommand(providerConfigureCmd, providerListCmd, providerVaultsCmd)
	rootCmd.AddCommand(providerCmd)
}
