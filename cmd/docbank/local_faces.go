package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/embedlocal"
)

func localFacesCommand() *cobra.Command {
	return &cobra.Command{
		Use: "setup-local", Args: cobra.NoArgs,
		Short: "Install and start Kit's local face model for this operating-system user",
		Long: "Install AuraFace-v1 and its detector, reporting the download size first.\n" +
			"Running this command approves that download and starts the shared local server.\n" +
			"An incompatible running server is reported and is never restarted automatically.\n" +
			"Run as the same operating-system user as the Docbank daemon.",
		Example: "  docbank photos faces setup-local",
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := embedlocal.DefaultDir()
			if err != nil {
				return fmt.Errorf("resolve local face model directory: %w", err)
			}
			return setupLocalFaces(cmd.Context(), cmd.OutOrStdout(), dir, embedlocal.Endpoint, embedlocal.Install)
		},
	}
}

func setupLocalFaces(ctx context.Context, out io.Writer, dir string,
	endpoint func(context.Context, embedlocal.Options) (string, error),
	install func(context.Context, embedlocal.InstallOptions) (embedlocal.Installation, error),
) error {
	opts := embedlocal.Options{DataDir: dir, Model: embedlocal.FaceModel, SkipOllama: true}
	_, err := endpoint(ctx, opts)
	if err != nil {
		if !errors.Is(err, embedlocal.ErrNotFound) {
			return fmt.Errorf("discover local face model: %w", err)
		}
		installed, err := install(ctx, embedlocal.InstallOptions{DataDir: dir, Model: embedlocal.FaceModel,
			Confirm: func(_ context.Context, size int64) (bool, error) {
				_, err := fmt.Fprintf(out, "Downloading local face model: %d bytes\n", size)
				if err != nil {
					return false, fmt.Errorf("report local face download: %w", err)
				}
				return true, nil
			},
		})
		if err != nil {
			return fmt.Errorf("install local face model: %w", err)
		}
		opts.Executable, opts.DataDir = installed.Executable, installed.DataDir
		if _, err := endpoint(ctx, opts); err != nil {
			return fmt.Errorf("start local face model: %w", err)
		}
	}
	_, err = fmt.Fprintln(out, "Local face model ready. Face comparisons still require explicit consent and review.")
	if err != nil {
		return fmt.Errorf("report local face model ready: %w", err)
	}
	return nil
}
