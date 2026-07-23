//go:build linux

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
	if err := exec.CommandContext(ctx, "xdg-open", rawURL).Run(); err != nil { //nolint:gosec // validated loopback URL
		return fmt.Errorf("running Linux browser opener: %w", err)
	}
	return nil
}
