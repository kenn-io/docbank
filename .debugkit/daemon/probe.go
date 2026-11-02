package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

// DefaultPingPath is the conventional liveness endpoint used by Probe.
const DefaultPingPath = "/api/ping"

// PingInfo is the common identity returned by daemon ping endpoints. OK must
// be true for a probe to succeed.
type PingInfo struct {
	OK      bool   `json:"ok"`
	Service string `json:"service,omitempty"`
	Version string `json:"version,omitempty"`
	PID     int    `json:"pid,omitempty"`
}

// PingHandlerOptions configures NewPingHandler.
type PingHandlerOptions struct {
	Service string
	Version string
	PID     int
}

// NewPingHandler returns a reference handler for DefaultPingPath-compatible
// daemon liveness endpoints.
func NewPingHandler(opts PingHandlerOptions) http.Handler {
	pid := opts.PID
	if pid == 0 {
		pid = os.Getpid()
	}
	info := PingInfo{
		OK:      true,
		Service: opts.Service,
		Version: opts.Version,
		PID:     pid,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(info)
	})
}

// ProbeOptions configures daemon liveness probes.
type ProbeOptions struct {
	Path            string
	ExpectedService string
	Timeout         time.Duration
}

func (o ProbeOptions) path() string {
	if o.Path == "" {
		return DefaultPingPath
	}
	return o.Path
}

func (o ProbeOptions) timeout() time.Duration {
	if o.Timeout == 0 {
		return time.Second
	}
	return o.Timeout
}

// Probe checks that ep answers its ping endpoint.
func Probe(ctx context.Context, ep Endpoint, opts ProbeOptions) (PingInfo, error) {
	if ep.Address == "" {
		return PingInfo{}, errors.New("empty daemon endpoint address")
	}
	client := ep.HTTPClient(HTTPClientOptions{
		Timeout:           opts.timeout(),
		DisableKeepAlives: true,
	})
	return ProbeHTTP(ctx, client, ep.BaseURL(), opts)
}

func probeRecordEndpoint(ctx context.Context, rec RuntimeRecord, proof *Proof, opts ProbeOptions) (PingInfo, error) {
	if proof != nil {
		return proof.Probe(ctx, rec, opts)
	}
	return Probe(ctx, rec.Endpoint(), opts)
}

// ProbeHTTP checks that baseURL answers its ping endpoint.
func ProbeHTTP(ctx context.Context, client *http.Client, baseURL string, opts ProbeOptions) (PingInfo, error) {
	info, _, err := probeHTTP(ctx, client, baseURL, opts, "")
	return info, err
}

func probeHTTP(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	opts ProbeOptions,
	proofChallenge string,
) (PingInfo, http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+opts.path(), nil)
	if err != nil {
		return PingInfo{}, nil, err
	}
	if proofChallenge != "" {
		req.Header.Set(proofChallengeHeader, proofChallenge)
	}
	resp, err := client.Do(req)
	if err != nil {
		return PingInfo{}, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return PingInfo{}, nil, fmt.Errorf("daemon ping returned %d", resp.StatusCode)
	}
	var info PingInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return PingInfo{}, nil, fmt.Errorf("decode daemon ping: %w", err)
	}
	if !info.OK {
		return PingInfo{}, nil, errors.New("daemon ping returned ok=false")
	}
	if opts.ExpectedService != "" && info.Service != opts.ExpectedService {
		return PingInfo{}, nil, fmt.Errorf("unexpected daemon service %q", info.Service)
	}
	return info, resp.Header.Clone(), nil
}

// DiscoverOptions configures runtime-file discovery.
type DiscoverOptions struct {
	Probe ProbeOptions
	// Proof enables proof-of-possession probing for each runtime record.
	Proof *Proof
	// RequirePIDAlive rejects dead processes and definite process-identity
	// mismatches. A record whose identity this caller can't judge is kept and
	// probed (see AGENTS.md). It also makes failed probes indeterminate:
	// Discover returns an UnreachableError when no later record succeeds. The
	// false zero value keeps the earlier compatibility behavior, where failed
	// probes are skipped and discovery can report definite absence.
	RequirePIDAlive bool
	Accept          func(RuntimeRecord, PingInfo) bool
}

// ErrDaemonUnreachable reports a live runtime record whose endpoint could not
// be proved usable. It classifies every Probe or proof-of-possession failure,
// including transport, HTTP, decoding, service-identity, and proof errors.
// Callers should inspect the wrapped error before choosing operator guidance.
var ErrDaemonUnreachable = errors.New("daemon is unreachable")

// UnreachableError retains the runtime and probe failure for a live daemon
// candidate that discovery could not safely select.
type UnreachableError struct {
	Record   RuntimeRecord
	Endpoint Endpoint
	Err      error
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("%v: pid %d at %s: %v",
		ErrDaemonUnreachable, e.Record.PID, e.Endpoint.ConfigAddress(), e.Err)
}

// Unwrap preserves both the classification and the endpoint probe failure.
func (e *UnreachableError) Unwrap() []error {
	if e.Err == nil {
		return []error{ErrDaemonUnreachable}
	}
	return []error{ErrDaemonUnreachable, e.Err}
}

// Discover scans runtime records and returns the first live daemon.
func Discover(ctx context.Context, store RuntimeStore, opts DiscoverOptions) (RuntimeRecord, PingInfo, bool, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeRecord{}, PingInfo{}, false, err
	}
	records, err := store.List()
	if err != nil {
		return RuntimeRecord{}, PingInfo{}, false, err
	}
	var unreachable error
	for _, rec := range records {
		if err := ctx.Err(); err != nil {
			return RuntimeRecord{}, PingInfo{}, false, err
		}
		if opts.RequirePIDAlive {
			if !runtimeRecordLive(rec) {
				continue
			}
		}
		ep := rec.Endpoint()
		info, err := probeRecordEndpoint(ctx, rec, opts.Proof, opts.Probe)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return RuntimeRecord{}, PingInfo{}, false, ctxErr
			}
			if opts.RequirePIDAlive && unreachable == nil {
				unreachable = &UnreachableError{
					Record:   rec,
					Endpoint: ep,
					Err:      err,
				}
			}
			continue
		}
		if opts.RequirePIDAlive && info.PID != rec.PID {
			continue
		}
		if opts.Accept != nil && !opts.Accept(rec, info) {
			continue
		}
		return rec, info, true, nil
	}
	return RuntimeRecord{}, PingInfo{}, false, unreachable
}
