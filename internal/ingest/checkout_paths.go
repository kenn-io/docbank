package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

// CheckoutPathOverlap protects reservations and working copies left by released checkouts.
// The retired result reports whether the matched checkout had already released retention,
// meaning the overlap is a leftover working copy rather than an active reservation.
func CheckoutPathOverlap(ctx context.Context, checkouts []store.PhotoCheckout, pathname string) (checkoutID string, overlap bool, retired bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var pending error
	var pendingID string
	for _, checkout := range checkouts {
		if err := ctx.Err(); err != nil {
			return "", false, false, err
		}
		overlaps, err := checkoutPathOverlap(ctx, checkout, pathname)
		if err != nil {
			pendingID = checkout.ID
			pending = errors.Join(pending, fmt.Errorf("checkout %s: %w", checkout.ID, err))
			continue
		}
		if overlaps {
			return checkout.ID, true, checkout.RetentionReleased, nil
		}
	}
	return pendingID, false, false, errors.Join(pending, ctx.Err())
}

func checkoutPathOverlap(ctx context.Context, checkout store.PhotoCheckout, pathname string) (bool, error) {
	if checkout.Destination == "" {
		return false, nil
	}
	if !checkout.RetentionReleased && (pathContains(pathname, checkout.Destination) || pathContains(checkout.Destination, pathname)) {
		return true, nil
	}
	info, err := os.Stat(checkout.Destination)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if checkout.RetentionReleased {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if info.IsDir() {
			dir, err := os.Open(checkout.Destination)
			if err != nil {
				return false, err
			}
			_, readErr := dir.Readdirnames(1)
			closeErr := dir.Close()
			if readErr != nil && readErr != io.EOF || closeErr != nil {
				return false, errors.Join(readErr, closeErr)
			}
			if readErr == io.EOF {
				return false, nil
			}
		}
	}
	overlap, err := PathsOverlapContext(ctx, pathname, checkout.Destination)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if !overlap && info != nil && info.IsDir() {
		_, overlap, err = WatchBindingOverlapContext(ctx, []config.WatchConfig{{Source: checkout.Destination}}, config.StoreBindingConfig{Kind: "filesystem", Path: pathname})
		if err != nil {
			return false, fmt.Errorf("checking checkout destination: %w", err)
		}
	}
	if !overlap {
		if candidate, statErr := os.Stat(pathname); statErr == nil && candidate.IsDir() {
			_, overlap, err = WatchBindingOverlapContext(ctx, []config.WatchConfig{{Source: pathname}}, config.StoreBindingConfig{Kind: "filesystem", Path: checkout.Destination})
			if err != nil {
				return false, err
			}
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return false, statErr
		}
	}
	return overlap, nil
}
