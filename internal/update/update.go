// Package update wraps kit/selfupdate with docbank's release identity and
// daemon-aware install: stop the daemon, swap the binary, restart it.
package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/kit/selfupdate"

	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/version"
)

// NewClient builds the selfupdate.Client for docbank's kenn-io/docbank
// releases, caching check results under cacheDir.
func NewClient(cacheDir string) selfupdate.Client {
	return selfupdate.Client{
		Owner: "kenn-io", Repo: "docbank", BinaryName: "docbank",
		CurrentVersion:         version.Version,
		CacheDir:               cacheDir,
		GitHubToken:            selfupdate.EnvironmentGitHubToken(),
		AllowUnsignedChecksums: true,
	}
}

// Options controls Run. Client, Root, and Destination are test seams; the
// zero value resolves each from the environment (default cache dir, vault
// home, and the running executable, respectively).
type Options struct {
	CheckOnly bool
	Yes       bool
	Force     bool
	// Confirm prompts the user; nil with Yes=false is an error (non-interactive).
	Confirm func(prompt string) (bool, error)

	// test seams:
	Client         *selfupdate.Client
	Root           string
	Destination    string
	WithLaunchLock func(context.Context, string, func() error) error
	Stop           func(context.Context, string) (bool, error)
	Start          func(context.Context, string) error
}

// Run checks for a newer docbank release and, unless CheckOnly, installs it:
// stopping a running daemon before the binary swap and restarting it after
// (with the new binary, since Start re-execs os.Executable()).
func Run(ctx context.Context, out io.Writer, opts Options) error {
	c := opts.Client
	if c == nil {
		layout, err := home.Resolve()
		if err != nil {
			return err
		}
		built := NewClient(filepath.Join(layout.Root, "cache", "update"))
		c = &built
	}
	root := opts.Root
	if root == "" {
		layout, err := home.Resolve()
		if err != nil {
			return err
		}
		root = layout.Root
	}

	info, err := c.Check(ctx, selfupdate.CheckOptions{Force: opts.Force})
	if err != nil {
		return fmt.Errorf("checking for updates: %w", err)
	}
	// A nil Info means kit decided no update should be offered (the current
	// version is already the latest release); CheckOptions.Force only
	// bypasses the check cache, it does not force a same-or-older release to
	// be offered.
	if info == nil {
		_, _ = fmt.Fprintf(out, "current: %s\n", c.CurrentVersion)
		_, _ = fmt.Fprintln(out, "already up to date")
		return nil
	}
	_, _ = fmt.Fprintf(out, "current: %s\nlatest:  %s\n", c.CurrentVersion, info.LatestVersion)
	if info.IsDevBuild && !opts.Force {
		_, _ = fmt.Fprintln(out, "dev build: pass --force to replace it")
		return nil
	}
	if opts.CheckOnly {
		return nil
	}
	if info.Checksum == "" {
		return errors.New("release has no SHA256 checksum; refusing to install")
	}
	_, _ = fmt.Fprintf(out, "download: %s (%s, sha256 %s)\n",
		info.AssetName, selfupdate.FormatSize(info.Size), info.Checksum)
	if !opts.Yes {
		if opts.Confirm == nil {
			return errors.New("confirmation required: pass --yes in non-interactive use")
		}
		ok, err := opts.Confirm(fmt.Sprintf("install %s?", info.LatestVersion))
		if err != nil {
			return err
		}
		if !ok {
			_, _ = fmt.Fprintln(out, "update aborted")
			return nil
		}
	}

	dest := opts.Destination
	if dest == "" {
		if dest, err = os.Executable(); err != nil {
			return fmt.Errorf("resolving current executable: %w", err)
		}
	}

	lockFn := opts.WithLaunchLock
	if lockFn == nil {
		lockFn = client.WithLaunchLock
	}
	stopFn := opts.Stop
	if stopFn == nil {
		stopFn = client.Stop
	}
	startFn := opts.Start
	if startFn == nil {
		startFn = func(ctx context.Context, root string) error {
			_, err := client.StartAnyVersion(ctx, root)
			return err
		}
	}

	// Daemon coordination: a running daemon serves from the old binary and
	// would version-mismatch every CLI call after the swap. Hold the same
	// launch lock Ensure uses, then stop, install, and restart with the new
	// executable before other CLI calls can auto-start an old daemon.
	return lockFn(ctx, root, func() error {
		wasRunning, err := stopFn(ctx, root)
		if err != nil {
			return fmt.Errorf("stopping daemon before update: %w", err)
		}
		if wasRunning {
			_, _ = fmt.Fprintln(out, "stopped running daemon")
		}
		if err := c.Install(ctx, info, selfupdate.InstallOptions{DestinationPath: dest}); err != nil {
			if wasRunning {
				if rerr := startFn(ctx, root); rerr != nil {
					return fmt.Errorf("install failed (%w) and daemon restart failed: %w", err, rerr)
				}
				_, _ = fmt.Fprintln(out, "install failed; restarted previous daemon")
			}
			return fmt.Errorf("installing %s: %w", info.LatestVersion, err)
		}
		_, _ = fmt.Fprintf(out, "installed %s -> %s\n", info.LatestVersion, dest)
		if wasRunning {
			if err := startFn(ctx, root); err != nil {
				return fmt.Errorf("daemon restart after update failed (start it with docbank daemon start): %w", err)
			}
			_, _ = fmt.Fprintln(out, "daemon restarted")
		}
		return nil
	})
}
