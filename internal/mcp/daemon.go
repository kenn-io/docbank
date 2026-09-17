package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"

	"go.kenn.io/docbank/internal/daemonconn"
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
	message    error
	facts      daemonconn.ProblemFacts
	diagnostic string
}

func (e *daemonBoundaryError) Error() string        { return e.message.Error() }
func (e *daemonBoundaryError) Unwrap() error        { return e.facts.MappedError }
func (e *daemonBoundaryError) Is(target error) bool { return target == e.message }

type daemonLease struct {
	mu         sync.Mutex
	daemonconn *daemonconn.Connection
	generation uint64
	acquiring  *daemonAcquisition
	acquireErr error
	ensure     func(context.Context) (*daemonconn.Connection, error)
	close      func(*daemonconn.Connection) error
	newContext func() (context.Context, context.CancelFunc)
	keyPolicy  daemonconn.APIKeyExclusionPolicy
}

type leasedDaemonClient struct {
	daemonconn *daemonconn.Connection
	generation uint64
}

type daemonAcquisition struct {
	done       chan struct{}
	daemonconn leasedDaemonClient
	err        error
}

func newDaemonLease() *daemonLease {
	return newDaemonLeaseWith(daemonconn.Ensure, func(c *daemonconn.Connection) error { return c.Close() })
}

func newDaemonLeaseWith(
	ensure func(context.Context) (*daemonconn.Connection, error),
	closeClient func(*daemonconn.Connection) error,
) *daemonLease {
	return newDaemonLeaseWithAcquisitionContext(ensure, closeClient, func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), daemonAcquisitionTimeout)
	})
}

func newDaemonLeaseWithAcquisitionContext(
	ensure func(context.Context) (*daemonconn.Connection, error),
	closeClient func(*daemonconn.Connection) error,
	newContext func() (context.Context, context.CancelFunc),
) *daemonLease {
	return &daemonLease{ensure: ensure, close: closeClient, newContext: newContext}
}

// bindAPIKeyExclusion fixes the HTTP credential policy before the listener is
// opened. Once bound, the policy gates every current and future daemon client.
func (lease *daemonLease) bindAPIKeyExclusion(policy daemonconn.APIKeyExclusionPolicy) error {
	if lease == nil || policy == nil {
		return errDaemonCredentialPolicy
	}
	lease.mu.Lock()
	if lease.keyPolicy != nil {
		lease.mu.Unlock()
		return errDaemonCredentialPolicy
	}
	lease.keyPolicy = policy
	current := lease.daemonconn
	rejected := current != nil && !policy.Allows(current)
	if rejected {
		lease.daemonconn = nil
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
	if lease.daemonconn != nil {
		current := leasedDaemonClient{daemonconn: lease.daemonconn, generation: lease.generation}
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
	if lease.generation != failed.generation || lease.daemonconn != failed.daemonconn {
		if lease.acquiring != nil {
			acquiring := lease.acquiring
			lease.mu.Unlock()
			return waitForDaemonAcquisition(ctx, acquiring)
		}
		current := leasedDaemonClient{daemonconn: lease.daemonconn, generation: lease.generation}
		acquireErr := lease.acquireErr
		lease.mu.Unlock()
		if current.daemonconn == nil {
			if acquireErr != nil {
				return leasedDaemonClient{}, acquireErr
			}
			return leasedDaemonClient{}, sanitizedDaemonError(errDaemonUnavailable,
				errors.New("daemon client was invalidated concurrently"))
		}
		return current, nil
	}
	lease.daemonconn = nil
	lease.generation++
	lease.acquireErr = nil
	acquiring := &daemonAcquisition{done: make(chan struct{})}
	lease.acquiring = acquiring
	lease.mu.Unlock()
	_ = lease.close(failed.daemonconn)
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
	var rejected *daemonconn.Connection
	lease.mu.Lock()
	if err == nil && lease.keyPolicy != nil && !lease.keyPolicy.Allows(c) {
		acquiring.err = errDaemonCredentialReuse
		lease.acquireErr = acquiring.err
		rejected = c
	} else if err == nil {
		lease.daemonconn = c
		lease.acquireErr = nil
		acquiring.daemonconn = leasedDaemonClient{daemonconn: c, generation: lease.generation}
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
		return acquiring.daemonconn, acquiring.err
	}
}

func (lease *daemonLease) discard(failed leasedDaemonClient) {
	lease.mu.Lock()
	if lease.generation != failed.generation || lease.daemonconn != failed.daemonconn {
		lease.mu.Unlock()
		return
	}
	lease.daemonconn = nil
	lease.generation++
	lease.acquireErr = nil
	lease.mu.Unlock()
	_ = lease.close(failed.daemonconn)
}

func daemonRead[T any](
	ctx context.Context,
	lease *daemonLease,
	read func(context.Context, *daemonconn.Connection) (T, error),
) (T, error) {
	var zero T
	current, err := lease.acquire(ctx)
	if err != nil {
		return zero, err
	}
	// A tool may make several daemon requests. Once any response starts, the
	// callback must not replay earlier reads such as cursor-page acquisition.
	var responseStarted atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotFirstResponseByte: func() { responseStarted.Store(true) },
	})
	result, err := read(ctx, current.daemonconn)
	if err == nil {
		return result, nil
	}
	if canceled := contextCancellation(ctx, err); canceled != nil {
		return zero, canceled
	}
	if !daemonconn.IsTransportError(err) {
		return zero, sanitizedDaemonError(errDaemonRequestFailed, err)
	}
	if responseStarted.Load() {
		lease.discard(current)
		return zero, sanitizedDaemonError(errDaemonRequestFailed, err)
	}

	replacement, replaceErr := lease.replace(ctx, current)
	if replaceErr != nil {
		return zero, replaceErr
	}
	result, err = read(ctx, replacement.daemonconn)
	if err == nil {
		return result, nil
	}
	if canceled := contextCancellation(ctx, err); canceled != nil {
		return zero, canceled
	}
	if daemonconn.IsTransportError(err) {
		lease.discard(replacement)
	}
	return zero, sanitizedDaemonError(errDaemonRequestFailed, err)
}

func daemonProcessingStart[T any](
	ctx context.Context,
	lease *daemonLease,
	start func(*daemonconn.Connection) (T, error),
) (T, error) {
	var zero T
	current, err := lease.acquire(ctx)
	if err != nil {
		return zero, err
	}
	result, err := start(current.daemonconn)
	if err == nil {
		return result, nil
	}
	if daemonconn.IsTransportError(err) || daemonconn.IsResponseDecodeError(err) {
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
	facts, _ := daemonconn.ExtractProblemFacts(cause)
	diagnostic := "daemon_request_failed"
	switch {
	case daemonconn.IsTransportError(cause):
		diagnostic = "daemon_transport_failure"
	case daemonconn.IsResponseDecodeError(cause):
		diagnostic = "daemon_invalid_response"
	}
	return &daemonBoundaryError{message: message, facts: facts, diagnostic: diagnostic}
}

func daemonProblemFacts(err error) (daemonconn.ProblemFacts, bool) {
	var boundary *daemonBoundaryError
	if !errors.As(err, &boundary) || boundary.facts.Code == "" {
		return daemonconn.ProblemFacts{}, false
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
