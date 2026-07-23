//go:build !darwin && !linux && !windows

package main

import (
	"context"
	"errors"
)

func openWebBrowser(_ context.Context, rawURL string) error {
	if err := validateWebURL(rawURL); err != nil {
		return err
	}
	return errors.New("opening a browser is unsupported on this platform; use --no-browser")
}
