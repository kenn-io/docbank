package embedserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/embedlocal"
	"go.kenn.io/kit/internal/embedassets"
)

// Options configures the foreground server. DataDir defaults to DefaultDir,
// Listen defaults to 127.0.0.1:0, and Library defaults beside the executable.
// Runtime records use the kit-embed prefix in DataDir.
type Options struct{ DataDir, Listen, Library string }

// DefaultDir returns the shared per-user embeddings data directory.
func DefaultDir() (string, error) {
	return embedlocal.DefaultDir()
}

// Serve prints its loopback URL to stderr after loading the model and publishing
// its runtime record. Cancellation closes connections and waits for active
// inference before native cleanup. Serve prints any returned error to stderr.
func Serve(ctx context.Context, opts Options) error {
	return serve(ctx, opts, loadModel, daemon.Endpoint.ListenContext)
}

func serve(ctx context.Context, opts Options, load func(context.Context, string, string) (engine, error), listen func(daemon.Endpoint, context.Context) (net.Listener, error)) (err error) {
	var release func()
	defer func() {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		if release != nil {
			release()
		}
	}()
	if opts.DataDir == "" {
		opts.DataDir, err = DefaultDir()
		if err != nil {
			return err
		}
	}
	opts.DataDir, err = filepath.Abs(opts.DataDir)
	if err != nil {
		return err
	}
	ep, err := daemon.ParseEndpoint(opts.Listen, daemon.ParseEndpointOptions{DefaultTCPAddress: "127.0.0.1:0", TCPPolicy: daemon.RequireLoopback})
	if err != nil {
		return err
	}
	if ep.IsUnix() {
		return errors.New("embeddings requires a loopback TCP endpoint")
	}
	store := daemon.RuntimeStore{Dir: opts.DataDir, Prefix: embedlocal.Service}
	lockCtx, cancelLock := context.WithTimeout(ctx, time.Second)
	release, err = store.AcquireOwnerLock(lockCtx)
	cancelLock()
	if err != nil {
		return fmt.Errorf("cannot own %s, another kennembed may be running: %w", opts.DataDir, err)
	}
	// The owner lock proves that servers which left records are gone.
	if _, err := store.CleanupDead(); err != nil {
		return fmt.Errorf("remove stale runtime records: %w", err)
	}
	if opts.Library == "" {
		executable, e := os.Executable()
		if e != nil {
			return e
		}
		opts.Library = filepath.Join(filepath.Dir(executable), embedassets.LibraryName())
	}
	if _, err := fmt.Fprintln(os.Stderr, "Loading nomic-embed-text model..."); err != nil {
		return err
	}
	engine, err := load(ctx, opts.DataDir, opts.Library)
	if err != nil {
		return err
	}
	var path string
	defer func() {
		err = errors.Join(err, engine.Close())
		if path != "" {
			err = errors.Join(err, os.Remove(path))
		}
	}()
	listener, err := listen(ep, ctx)
	if err != nil {
		return err
	}
	defer listener.Close()
	ep.Address = listener.Addr().String()
	record := daemon.NewRuntimeRecord(embedlocal.Service, embedlocal.Version, ep)
	ping := daemon.NewPingHandler(daemon.PingHandlerOptions{Service: record.Service, Version: record.Version, PID: record.PID})
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	embeddings := &handler{engine: engine, gate: make(chan struct{}, 1), log: os.Stderr}
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: time.Minute, BaseContext: func(net.Listener) context.Context { return ctx },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/ping":
				ping.ServeHTTP(w, r)
			case "/v1/embeddings":
				if r.Method != http.MethodPost {
					w.Header().Set("Allow", "POST")
					http.Error(w, "POST required", http.StatusMethodNotAllowed)
					return
				}
				embeddings.ServeHTTP(w, r)
			default:
				http.NotFound(w, r)
			}
		}),
	}
	path, err = store.Write(record)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(os.Stderr, ep.BaseURL()+"/v1"); err != nil {
		return err
	}
	shutdown := make(chan error, 1)
	go func() { <-ctx.Done(); shutdown <- server.Close() }()
	err = server.Serve(listener)
	cancel()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	err = errors.Join(err, <-shutdown)
	// A synchronous native call owns the model until it returns, even after
	// Close has interrupted its HTTP connection. Hold the gate through cleanup.
	embeddings.gate <- struct{}{}
	return err
}
