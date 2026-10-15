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

// CheckoutPathOverlap protects reservations and working copies left by
// released checkouts. It scans each checkout's destination tree, so its
// running time follows the caller's own context: a fixed inner deadline
// here would cut off a traversal that is still making progress on a large
// library, turning a slow-but-successful check into a permanent failure
// that a caller's retry loop can never outrun. The retired result reports
// whether the matched checkout had already released retention, meaning
// the overlap is a leftover working copy rather than an active reservation.
func CheckoutPathOverlap(ctx context.Context, checkouts []store.PhotoCheckout, pathname string) (checkoutID string, overlap bool, retired bool, err error) {
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
		if !info.IsDir() {
			// Something other than the checkout's folder now occupies this
			// path, so it no longer holds a copy and imposes no admission
			// restriction, whether or not it would even be readable.
			return false, nil
		}
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
	return CheckoutDestinationRelatesToPath(ctx, checkout.Destination, pathname)
}

// CheckoutDestinationRelatesToPath reports whether pathname is the same
// location as, nested beneath or above, or a filesystem-equivalent alias
// (such as a bind mount) of destination. It always runs the full identity
// traversal, including through WatchBindingOverlapContext's directory-tree
// walk, regardless of whether destination currently holds any files — the
// shortcut checkoutPathOverlap takes for an empty, released destination
// skips that traversal because an empty folder restricts nothing, but a
// caller deciding whether destination is the same folder as pathname (to
// release its recorded protection on reuse) needs the real relationship,
// not just the absence of a restriction.
func CheckoutDestinationRelatesToPath(ctx context.Context, destination, pathname string) (bool, error) {
	overlap, err := PathsOverlapContext(ctx, pathname, destination)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if !overlap {
		if info, statErr := os.Stat(destination); statErr == nil && info.IsDir() {
			_, overlap, err = WatchBindingOverlapContext(ctx, []config.WatchConfig{{Source: destination}}, config.StoreBindingConfig{Kind: "filesystem", Path: pathname})
			if err != nil {
				return false, fmt.Errorf("checking checkout destination: %w", err)
			}
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return false, statErr
		}
	}
	if !overlap {
		if candidate, statErr := os.Stat(pathname); statErr == nil && candidate.IsDir() {
			_, overlap, err = WatchBindingOverlapContext(ctx, []config.WatchConfig{{Source: pathname}}, config.StoreBindingConfig{Kind: "filesystem", Path: destination})
			if err != nil {
				return false, err
			}
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return false, statErr
		}
	}
	return overlap, nil
}
