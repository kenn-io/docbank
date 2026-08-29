package mcp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.kenn.io/docbank/internal/client"
)

const daemonAcquisitionTimeout = 45 * time.Second

var (
	errDaemonUnavailable        = errors.New("local Docbank daemon is unavailable")
	errDaemonRequestFailed      = errors.New("local Docbank daemon request failed")
	errDaemonCredentialReuse    = errors.New("MCP HTTP credential must differ from the daemon API key")
	errDaemonCredentialPolicy   = errors.New("daemon credential exclusion policy is already configured")
	errProcessingOutcomeUnknown = errors.New(
		"processing request outcome is unknown; check processing status before retrying")
)

type daemonBoundaryError struct {
	message error
	facts   client.ProblemFacts
}

func (e *daemonBoundaryError) Error() string        { return e.message.Error() }
func (e *daemonBoundaryError) Unwrap() error        { return e.facts.MappedError }
func (e *daemonBoundaryError) Is(target error) bool { return target == e.message }

type daemonLease struct {
	mu         sync.Mutex
	client     *client.Client
	generation uint64
	acquiring  *daemonAcquisition
	acquireErr error
	ensure     func(context.Context) (*client.Client, error)
	close      func(*client.Client) error
	newContext func() (context.Context, context.CancelFunc)
	keyPolicy  client.APIKeyExclusionPolicy
}

type leasedDaemonClient struct {
	client     *client.Client
	generation uint64
}

type daemonAcquisition struct {
	done   chan struct{}
	client leasedDaemonClient
	err    error
}

func newDaemonLease() *daemonLease {
	return newDaemonLeaseWith(client.Ensure, func(c *client.Client) error { return c.Close() })
}

func newDaemonLeaseWith(
	ensure func(context.Context) (*client.Client, error),
	closeClient func(*client.Client) error,
) *daemonLease {
	return newDaemonLeaseWithAcquisitionContext(ensure, closeClient, func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), daemonAcquisitionTimeout)
	})
}

func newDaemonLeaseWithAcquisitionContext(
	ensure func(context.Context) (*client.Client, error),
	closeClient func(*client.Client) error,
	newContext func() (context.Context, context.CancelFunc),
) *daemonLease {
	return &daemonLease{ensure: ensure, close: closeClient, newContext: newContext}
}

// bindAPIKeyExclusion fixes the HTTP credential policy before the listener is
// opened. Once bound, the policy gates every current and future daemon client.
func (lease *daemonLease) bindAPIKeyExclusion(policy client.APIKeyExclusionPolicy) error {
	if lease == nil || policy == nil {
		return errDaemonCredentialPolicy
	}
	lease.mu.Lock()
	if lease.keyPolicy != nil {
		lease.mu.Unlock()
		return errDaemonCredentialPolicy
	}
	lease.keyPolicy = policy
	current := lease.client
	rejected := current != nil && !policy.Allows(current)
	if rejected {
		lease.client = nil
		lease.generation++
		lease.acquireErr = errDaemonCredentialReuse
	}
	lease.mu.Unlock()
	if rejected {
		_ = lease.close(current)
		return errDaemonCredentialReuse
	}
	return nil
}

func (lease *daemonLease) acquire(ctx context.Context) (leasedDaemonClient, error) {
	if err := contextCancellation(ctx, nil); err != nil {
		return leasedDaemonClient{}, err
	}
	lease.mu.Lock()
	if lease.client != nil {
		current := leasedDaemonClient{client: lease.client, generation: lease.generation}
		lease.mu.Unlock()
		return current, nil
	}
	if lease.acquiring != nil {
		acquiring := lease.acquiring
		lease.mu.Unlock()
		return waitForDaemonAcquisition(ctx, acquiring)
	}
	lease.generation++
	lease.acquireErr = nil
	acquiring := &daemonAcquisition{done: make(chan struct{})}
	lease.acquiring = acquiring
	lease.mu.Unlock()
	go lease.completeAcquisition(acquiring)
	return waitForDaemonAcquisition(ctx, acquiring)
}

func (lease *daemonLease) replace(
	ctx context.Context, failed leasedDaemonClient,
) (leasedDaemonClient, error) {
	if err := contextCancellation(ctx, nil); err != nil {
		return leasedDaemonClient{}, err
	}
	lease.mu.Lock()
	if lease.generation != failed.generation || lease.client != failed.client {
		if lease.acquiring != nil {
			acquiring := lease.acquiring
			lease.mu.Unlock()
			return waitForDaemonAcquisition(ctx, acquiring)
		}
		current := leasedDaemonClient{client: lease.client, generation: lease.generation}
		acquireErr := lease.acquireErr
		lease.mu.Unlock()
		if current.client == nil {
			if acquireErr != nil {
				return leasedDaemonClient{}, acquireErr
			}
			return leasedDaemonClient{}, sanitizedDaemonError(errDaemonUnavailable,
				errors.New("daemon client was invalidated concurrently"))
		}
		return current, nil
	}
	lease.client = nil
	lease.generation++
	lease.acquireErr = nil
	acquiring := &daemonAcquisition{done: make(chan struct{})}
	lease.acquiring = acquiring
	lease.mu.Unlock()
	_ = lease.close(failed.client)
	go lease.completeAcquisition(acquiring)
	return waitForDaemonAcquisition(ctx, acquiring)
}

func (lease *daemonLease) completeAcquisition(acquiring *daemonAcquisition) {
	ctx, cancel := lease.newContext()
	defer cancel()
	c, err := lease.ensure(ctx)
	if err == nil && c == nil {
		err = errors.New("daemon acquisition returned no client")
	}
	var rejected *client.Client
	lease.mu.Lock()
	if err == nil && lease.keyPolicy != nil && !lease.keyPolicy.Allows(c) {
		acquiring.err = errDaemonCredentialReuse
		lease.acquireErr = acquiring.err
		rejected = c
	} else if err == nil {
		lease.client = c
		lease.acquireErr = nil
		acquiring.client = leasedDaemonClient{client: c, generation: lease.generation}
	} else {
		acquiring.err = sanitizedDaemonError(errDaemonUnavailable, err)
		lease.acquireErr = acquiring.err
	}
	lease.acquiring = nil
	close(acquiring.done)
	lease.mu.Unlock()
	if rejected != nil {
		_ = lease.close(rejected)
	}
}

func waitForDaemonAcquisition(
	ctx context.Context, acquiring *daemonAcquisition,
) (leasedDaemonClient, error) {
	select {
	case <-ctx.Done():
		return leasedDaemonClient{}, contextCancellation(ctx, nil)
	case <-acquiring.done:
		if err := contextCancellation(ctx, nil); err != nil {
			return leasedDaemonClient{}, err
		}
		return acquiring.client, acquiring.err
	}
}

func (lease *daemonLease) discard(failed leasedDaemonClient) {
	lease.mu.Lock()
	if lease.generation != failed.generation || lease.client != failed.client {
		lease.mu.Unlock()
		return
	}
	lease.client = nil
	lease.generation++
	lease.acquireErr = nil
	lease.mu.Unlock()
	_ = lease.close(failed.client)
}

func daemonRead[T any](
	ctx context.Context,
	lease *daemonLease,
	read func(*client.Client) (T, error),
) (T, error) {
	var zero T
	current, err := lease.acquire(ctx)
	if err != nil {
		return zero, err
	}
	result, err := read(current.client)
	if err == nil {
		return result, nil
	}
	if canceled := contextCancellation(ctx, err); canceled != nil {
		return zero, canceled
	}
	if !client.IsTransportError(err) {
		return zero, sanitizedDaemonError(errDaemonRequestFailed, err)
	}

	replacement, replaceErr := lease.replace(ctx, current)
	if replaceErr != nil {
		return zero, replaceErr
	}
	result, err = read(replacement.client)
	if err == nil {
		return result, nil
	}
	if canceled := contextCancellation(ctx, err); canceled != nil {
		return zero, canceled
	}
	if client.IsTransportError(err) {
		lease.discard(replacement)
	}
	return zero, sanitizedDaemonError(errDaemonRequestFailed, err)
}

func daemonProcessingStart[T any](
	ctx context.Context,
	lease *daemonLease,
	start func(*client.Client) (T, error),
) (T, error) {
	var zero T
	current, err := lease.acquire(ctx)
	if err != nil {
		return zero, err
	}
	result, err := start(current.client)
	if err == nil {
		return result, nil
	}
	if client.IsTransportError(err) || client.IsResponseDecodeError(err) {
		lease.discard(current)
		return zero, sanitizedDaemonError(errProcessingOutcomeUnknown, err)
	}
	if canceled := contextCancellation(ctx, err); canceled != nil {
		lease.discard(current)
		return zero, sanitizedDaemonError(errProcessingOutcomeUnknown, canceled)
	}
	return zero, sanitizedDaemonError(errDaemonRequestFailed, err)
}

func sanitizedDaemonError(message, cause error) error {
	facts, _ := client.ExtractProblemFacts(cause)
	return &daemonBoundaryError{message: message, facts: facts}
}

func daemonProblemFacts(err error) (client.ProblemFacts, bool) {
	var boundary *daemonBoundaryError
	if !errors.As(err, &boundary) || boundary.facts.Code == "" {
		return client.ProblemFacts{}, false
	}
	return boundary.facts, true
}

func contextCancellation(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("daemon request canceled: %w", ctxErr)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("daemon request canceled: %w", context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("daemon request canceled: %w", context.DeadlineExceeded)
	}
	return nil
}
