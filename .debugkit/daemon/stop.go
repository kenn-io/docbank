package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrStopProofRequired means a TCP shutdown request needs endpoint proof.
var ErrStopProofRequired = errors.New("daemon stop requires endpoint proof")

// ErrExitUnconfirmed means the recorded process is gone but exit evidence is inconclusive.
var ErrExitUnconfirmed = errors.New("daemon exit is unconfirmed")

// StopOptions configures a shutdown request and the wait for daemon exit.
type StopOptions struct {
	Probe ProbeOptions
	// Proof is required for TCP records in every process scope.
	Proof *Proof
	// Request asks the verified daemon to shut down.
	// Its error ends Stop and is returned wrapped. Treat a connection dropped
	// during daemon shutdown as success so Stop can wait for exit.
	Request func(context.Context, RuntimeRecord) error
	// Exited supplies the caller's exit proof. When set, only a true result
	// confirms exit; a refused connection or another answering PID keeps waiting.
	// Without Exited, connection refusal ends the wait even if the process is
	// still draining. Set Exited when the caller needs to wait for process exit.
	Exited func(context.Context, RuntimeRecord) (bool, error)
	// Timeout bounds the whole call. Zero or negative values use 30 seconds.
	Timeout time.Duration
}

// Stop verifies rec's endpoint, requests shutdown, and waits for exit.
// TCP records require Proof in every process scope; Unix records may use a PID ping.
// When rec's process is proven gone, Stop checks exit evidence without a request.
// Without Exited, a stale Unix socket also confirms exit before any request.
// A record with no versioned identity is judged local, as in Discover.
// Without Exited, a refused connection or a replacement answering with a positive
// PID other than the recorded one ends the wait.
// Connection refusal doesn't prove process exit; the process may still be
// draining. Callers that need process exit supply Exited.
// A replacement answering with the recorded PID looks like the original, so
// the wait ends at its bound. Two PID namespaces can each have a daemon at PID 2.
// Callers that need to distinguish these replacements supply Exited.
// Stop sends no signals and removes no files.
func Stop(ctx context.Context, rec RuntimeRecord, opts StopOptions) error {
	if rec.PID <= 0 {
		return fmt.Errorf("daemon stop requires a positive PID: %d", rec.PID)
	}
	if opts.Request == nil {
		return errors.New("daemon stop request is required")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ep := rec.Endpoint()
	if !runtimeRecordLive(rec) {
		exited, err := stopExitConfirmed(ctx, rec, opts)
		if err != nil {
			return err
		}
		if exited {
			return nil
		}
		return fmt.Errorf("stop daemon at %s: %w", ep.Address, ErrExitUnconfirmed)
	}
	if ep.Network != NetworkUnix && opts.Proof == nil {
		return stopErrorUnlessExited(ctx, rec, opts, fmt.Errorf("stop daemon at %s: %w", ep.Address, ErrStopProofRequired))
	}
	info, err := probeRecordEndpoint(ctx, rec, opts.Proof, opts.Probe)
	if err == nil && opts.Proof == nil && info.PID != rec.PID {
		err = fmt.Errorf("daemon ping returned PID %d", info.PID)
	}
	if err != nil {
		return stopErrorUnlessExited(ctx, rec, opts, &UnreachableError{Record: rec, Endpoint: ep, Err: err})
	}
	if err := opts.Request(ctx, rec); err != nil {
		return fmt.Errorf("request daemon shutdown: %w", err)
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait for daemon exit: %w", err)
		}
		exited, err := stopExitConfirmed(ctx, rec, opts)
		if err != nil {
			return err
		}
		if exited {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for daemon exit: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func stopErrorUnlessExited(ctx context.Context, rec RuntimeRecord, opts StopOptions, stopErr error) error {
	if opts.Exited != nil {
		exited, err := stopExitConfirmed(ctx, rec, opts)
		if err != nil {
			return errors.Join(stopErr, err)
		}
		if exited {
			return nil
		}
	} else if rec.Endpoint().Network == NetworkUnix && isStaleUnixSocketDialError(stopErr) {
		return nil
	}
	return stopErr
}

func stopExitConfirmed(ctx context.Context, rec RuntimeRecord, opts StopOptions) (bool, error) {
	if opts.Exited != nil {
		exited, err := opts.Exited(ctx, rec)
		if err != nil {
			return false, fmt.Errorf("check daemon exit: %w", err)
		}
		return exited, nil
	}
	// A replacement with another service must also confirm exit.
	opts.Probe.ExpectedService = ""
	info, err := Probe(ctx, rec.Endpoint(), opts.Probe)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, fmt.Errorf("check daemon exit: %w", ctxErr)
	}
	return (err == nil && info.PID > 0 && info.PID != rec.PID) || isStaleUnixSocketDialError(err), nil
}
