//go:build windows

package main

import (
	"context"
	"fmt"
	"os/exec"
)

func openWebBrowser(ctx context.Context, rawURL string) error {
	if err := validateWebURL(rawURL); err != nil {
		return err
	}
	if err := exec.CommandContext( //nolint:gosec // validated loopback URL
		ctx, "rundll32", "url.dll,FileProtocolHandler", rawURL,
	).Run(); err != nil {
		return fmt.Errorf("running Windows browser opener: %w", err)
	}
	return nil
}
