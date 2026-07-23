//go:build darwin

package main

import (
	"context"
	"fmt"
	"os/exec"
)

func openWebBrowser(ctx context.Context, rawURL string) error {
	if err := validateWebLaunchURL(rawURL); err != nil {
		return err
	}
	if err := exec.CommandContext(ctx, "open", rawURL).Run(); err != nil { //nolint:gosec // validated loopback URL
		return fmt.Errorf("running macOS browser opener: %w", err)
	}
	return nil
}
