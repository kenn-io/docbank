package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"github.com/cenkalti/backoff/v7"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/store"
)

// CheckoutPathOverlap protects reservations and working copies left by
// released checkouts. It scans each checkout's destination tree once and
// pathname's tree once for all checkouts, so its running time follows the
// caller's own context: a fixed inner deadline here would cut off a traversal
// that is still making progress on a large library, turning a slow-but-
// successful check into a permanent failure that a caller's retry loop can
// never outrun.
func CheckoutPathOverlap(ctx context.Context, checkouts []store.PhotoCheckout, pathname string) (checkoutID string, overlap bool, err error) {
	candidates := make([]store.PhotoCheckout, 0, len(checkouts))
	for _, checkout := range checkouts {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		if !checkoutPathProtection(checkout, pathname) {
			continue
		}
		if checkout.DestinationReserved && (pathContains(pathname, checkout.Destination) || pathContains(checkout.Destination, pathname)) {
			return checkout.ID, true, nil
		}
		candidates = append(candidates, checkout)
	}
	checks, err := checkoutDestinationsRelateToPath(ctx, candidates, pathname)
	if err != nil {
		return "", false, err
	}
	var pending error
	var pendingID string
	for i, check := range checks {
		if check.related {
			return candidates[i].ID, true, nil
		}
		if check.err != nil {
			pendingID = candidates[i].ID
			pending = errors.Join(pending, fmt.Errorf("checkout %s: %w", candidates[i].ID, check.err))
		}
	}
	return pendingID, false, errors.Join(pending, ctx.Err())
}

func checkoutPathProtection(checkout store.PhotoCheckout, pathname string) bool {
	protected, err := PhotoCheckoutProtectsDestination(checkout)
	if err != nil {
		slog.Warn("checkout destination cannot be inspected; retaining path protection", "checkout", checkout.ID, "destination", checkout.Destination, "path", pathname, "error", err)
		return true
	}
	return protected
}

// RelatedCheckouts returns the checkouts whose destination is pathname,
// nested beneath or above it, or a filesystem-equivalent alias (such as a bind
// mount). It always runs the full identity traversal, including for an empty
// destination: CheckoutPathOverlap skips an empty, released destination
// because it restricts nothing, but a caller deciding whether a destination
// is the same folder as pathname (to release its recorded protection on
// reuse) needs the real relationship. Checkouts whose relationship cannot be
// confirmed are omitted.
func RelatedCheckouts(ctx context.Context, checkouts []store.PhotoCheckout, pathname string) ([]store.PhotoCheckout, error) {
	checks, err := checkoutDestinationsRelateToPath(ctx, checkouts, pathname)
	if err != nil {
		return nil, err
	}
	var related []store.PhotoCheckout
	for i, check := range checks {
		if check.related {
			related = append(related, checkouts[i])
		}
	}
	return related, nil
}

// ReleaseClearedCheckoutDestinations persists folder reuse after separation succeeds.
func ReleaseClearedCheckoutDestinations(ctx context.Context, catalog *store.Store, checkouts []store.PhotoCheckout, pathname string) error {
	var released []store.PhotoCheckout
	for _, checkout := range checkouts {
		if !checkout.RetentionReleased || checkout.Destination == "" {
			continue
		}
		if protected, err := PhotoCheckoutProtectsDestination(checkout); err != nil || protected {
			continue
		}
		released = append(released, checkout)
	}
	related, err := RelatedCheckouts(ctx, released, pathname)
	if err != nil {
		return err
	}
	for _, checkout := range related {
		if err := catalog.ReleaseCheckoutDestination(ctx, checkout.ID); err != nil {
			return err
		}
	}
	return nil
}

type destinationCheck struct {
	related bool
	err     error
}

// checkoutDestinationsRelateToPath walks each destination's tree once for
// pathname, then pathname's tree once for every remaining destination. The
// returned error reports only cancellation; per-checkout failures are in the
// checks.
func checkoutDestinationsRelateToPath(ctx context.Context, checkouts []store.PhotoCheckout, pathname string) ([]destinationCheck, error) {
	checks := make([]destinationCheck, len(checkouts))
	var remaining []int
	var targets []fs.FileInfo
	for i, checkout := range checkouts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		destination := checkout.Destination
		overlap, err := PathsOverlapContext(ctx, pathname, destination)
		if err != nil && !CheckoutPathMissing(err) {
			checks[i].err = err
			continue
		}
		if !overlap {
			if info, statErr := os.Stat(destination); statErr == nil && info.IsDir() {
				overlap, err = existingPathPrefixMatchesWatch(ctx, pathname, config.WatchConfig{Source: destination})
				if err != nil {
					checks[i].err = fmt.Errorf("checking checkout destination: %w", err)
					continue
				}
			} else if statErr != nil && !CheckoutPathMissing(statErr) {
				checks[i].err = statErr
				continue
			}
		}
		if overlap {
			checks[i].related = true
			continue
		}
		target, ok, err := existingDirectoryPrefix(ctx, destination)
		if err != nil {
			checks[i].err = err
			continue
		}
		if ok {
			remaining = append(remaining, i)
			targets = append(targets, target)
		}
	}
	if len(remaining) == 0 {
		return checks, ctx.Err()
	}
	candidate, statErr := os.Stat(pathname)
	if statErr != nil || !candidate.IsDir() {
		if statErr != nil && !CheckoutPathMissing(statErr) {
			for _, i := range remaining {
				checks[i].err = statErr
			}
		}
		return checks, ctx.Err()
	}
	watch := config.WatchConfig{Source: pathname}
	contains, err := watchTreeContainsAny(ctx, watch, targets)
	if err != nil {
		for _, i := range remaining {
			checks[i].err = err
		}
		return checks, ctx.Err()
	}
	if !contains {
		return checks, ctx.Err()
	}
	// Overlap is rare; identify the matching destinations one at a time.
	for n, i := range remaining {
		checks[i].related, checks[i].err = watchTreeContainsAny(ctx, watch, targets[n:n+1])
	}
	return checks, ctx.Err()
}

// WaitForCheckoutSeparation retries until the path clears or the caller cancels.
// It reports each changed failure once and caps the delay at thirty seconds.
func WaitForCheckoutSeparation(ctx context.Context, check func(context.Context) error, notify func(error)) error {
	delay := backoff.NewExponentialBackOff()
	delay.InitialInterval = time.Second
	delay.MaxInterval = 30 * time.Second
	delay.RandomizationFactor = 0
	var previous string
	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		return struct{}{}, check(ctx)
	}, backoff.WithBackOff(delay), backoff.WithMaxElapsedTime(0), backoff.WithNotify(func(err error, _ time.Duration) {
		if message := err.Error(); message != previous {
			notify(err)
			previous = message
		}
	}))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return backoff.AsRetryError(err).LastErr
	}
	return nil
}
