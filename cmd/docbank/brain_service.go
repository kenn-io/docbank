package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/brainclient"
	"go.kenn.io/docbank/internal/brainservice"
)

func brainServiceSpec(ctx context.Context, name, root string) (brainservice.Spec, error) {
	if err := ctx.Err(); err != nil {
		return brainservice.Spec{}, err
	}
	if root == "" {
		var err error
		root, err = brainclient.DefaultConfigRoot()
		if err != nil {
			return brainservice.Spec{}, err
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return brainservice.Spec{}, err
	}
	current, err := user.Current()
	if err != nil {
		return brainservice.Spec{}, fmt.Errorf("resolving current user: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return brainservice.Spec{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return brainservice.Spec{}, err
	}
	config, err := os.UserConfigDir()
	if err != nil {
		return brainservice.Spec{}, err
	}
	config, err = filepath.Abs(config)
	if err != nil {
		return brainservice.Spec{}, err
	}
	return brainservice.Spec{Name: name, Executable: executable, ConfigRoot: root, UserID: current.Uid, UserHome: home, UserConfigRoot: config}, nil
}

func newBrainLifecycleCommands(registry func() (*brainclient.Registry, error), configRoot func() string, driver brainservice.Driver) (*cobra.Command, *cobra.Command) {
	disconnect := &cobra.Command{Use: "disconnect <name>", Short: "Remove stopped client settings", Example: "  docbank brain disconnect notes", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		r, err := registry()
		if err != nil {
			return err
		}
		spec, err := brainServiceSpec(cmd.Context(), args[0], configRoot())
		if err != nil {
			return err
		}
		client := brainclient.New(r, brainclient.Dependencies{DisconnectGuard: func(ctx context.Context, _ string, remove func(context.Context) error) error {
			return driver.WithAbsent(ctx, spec, remove)
		}})
		if err := client.Disconnect(cmd.Context(), args[0]); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Connection removed. Published and archived documents remain."); err != nil {
			return fmt.Errorf("writing disconnect result: %w", err)
		}
		return nil
	}}
	service := &cobra.Command{Use: "service", Short: "Manage the per-user publisher launcher", Long: "Install, inspect, or remove the current user's publisher launcher."}
	var dryRun bool
	install := &cobra.Command{Use: "install <name>", Short: "Install the per-user launcher", Example: "  docbank brain service install notes --dry-run", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		r, err := registry()
		if err != nil {
			return err
		}
		spec, err := brainServiceSpec(cmd.Context(), args[0], configRoot())
		if err != nil {
			return err
		}
		check := func(ctx context.Context) error {
			cfg, err := r.Get(ctx, args[0])
			if err != nil {
				return err
			}
			if cfg.RemoteOrigin != "" {
				if cfg.KeyFile == "" {
					return errors.New("a remote background publisher requires an existing API key file")
				}
				if _, err := brainclient.ReadKey(cfg.KeyFile); err != nil {
					return err
				}
			}
			return nil
		}
		if dryRun {
			if err := check(cmd.Context()); err != nil {
				return err
			}
			registration, err := brainservice.Render(runtime.GOOS, spec)
			if err != nil {
				return err
			}
			return writeBrainJSON(cmd.OutOrStdout(), registration)
		}
		status, err := driver.InstallChecked(cmd.Context(), spec, check)
		if err != nil {
			return err
		}
		return writeBrainJSON(cmd.OutOrStdout(), status)
	}}
	install.Flags().BoolVar(&dryRun, "dry-run", false, "show the registration without installing it")
	status := &cobra.Command{Use: "status <name>", Short: "Inspect the per-user launcher", Example: "  docbank brain service status notes", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		spec, err := brainServiceSpec(cmd.Context(), args[0], configRoot())
		if err != nil {
			return err
		}
		value, err := driver.Status(cmd.Context(), spec)
		if err != nil {
			return err
		}
		return writeBrainJSON(cmd.OutOrStdout(), value)
	}}
	uninstall := &cobra.Command{Use: "uninstall <name>", Short: "Remove the owned launcher", Example: "  docbank brain service uninstall notes", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		spec, err := brainServiceSpec(cmd.Context(), args[0], configRoot())
		if err != nil {
			return err
		}
		if err := driver.Uninstall(cmd.Context(), spec); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Launcher removed. Connection settings and archived documents remain."); err != nil {
			return fmt.Errorf("writing launcher removal result: %w", err)
		}
		return nil
	}}
	service.AddCommand(install, status, uninstall)
	return disconnect, service
}
