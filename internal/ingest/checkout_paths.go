package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

// CheckoutPathOverlap protects reservations and working copies left by released checkouts.
// The retired result reports whether the matched checkout had already released retention,
// meaning the overlap is a leftover working copy rather than an active reservation.
func CheckoutPathOverlap(ctx context.Context, checkouts []store.PhotoCheckout, pathname string) (checkoutID string, overlap bool, retired bool, err error) {
	for _, checkout := range checkouts {
		if err := ctx.Err(); err != nil {
			return "", false, false, err
		}
		if checkout.Destination == "" {
			continue
		}
		info, err := os.Stat(checkout.Destination)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return checkout.ID, false, false, err
		}
		if checkout.RetentionReleased {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if info.IsDir() {
				dir, err := os.Open(checkout.Destination)
				if err != nil {
					return checkout.ID, false, false, err
				}
				_, readErr := dir.Readdirnames(1)
				closeErr := dir.Close()
				if readErr != nil && readErr != io.EOF || closeErr != nil {
					return checkout.ID, false, false, errors.Join(readErr, closeErr)
				}
				if readErr == io.EOF {
					continue
				}
			}
		}
		overlaps, err := PathsOverlapContext(ctx, pathname, checkout.Destination)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return checkout.ID, false, false, err
		}
		if !overlaps && info != nil && info.IsDir() {
			_, overlaps, err = WatchBindingOverlapContext(ctx, []config.WatchConfig{{Source: checkout.Destination}}, config.StoreBindingConfig{Kind: "filesystem", Path: pathname})
			if err != nil {
				return checkout.ID, false, false, fmt.Errorf("checking checkout destination: %w", err)
			}
		}
		if !overlaps {
			if candidate, statErr := os.Stat(pathname); statErr == nil && candidate.IsDir() {
				_, overlaps, err = WatchBindingOverlapContext(ctx, []config.WatchConfig{{Source: pathname}}, config.StoreBindingConfig{Kind: "filesystem", Path: checkout.Destination})
				if err != nil {
					return checkout.ID, false, false, err
				}
			} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				return checkout.ID, false, false, statErr
			}
		}
		if overlaps {
			return checkout.ID, true, checkout.RetentionReleased, nil
		}
	}
	return "", false, false, ctx.Err()
}
