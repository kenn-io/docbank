// Command docbank-conversion-worker serves a bounded private conversion spool.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"go.kenn.io/docbank/internal/conversionworker"
)

func main() {
	// This standalone service owns the root lifetime; requests derive from it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stderr); err != nil && !errors.Is(err, context.Canceled) {
		_, _ = fmt.Fprintln(os.Stderr, err)
		stop()
		os.Exit(1)
	}
	stop()
}

func run(ctx context.Context, args []string, environment func(string) string, output io.Writer) (retErr error) {
	flags := flag.NewFlagSet("docbank-conversion-worker", flag.ContinueOnError)
	flags.SetOutput(output)
	var options conversionworker.Options
	var listen string
	flags.StringVar(&options.Directory, "directory", "", "private worker spool (required)")
	flags.StringVar(&options.BackendURL, "backend", "", "exclusive numeric loopback Docling origin (required)")
	flags.StringVar(&listen, "listen", "127.0.0.1:5002", "explicit loopback, private, or shared-network IP and port")
	flags.Int64Var(&options.MaxSourceBytes, "max-source-bytes", 64<<20, "source byte limit, at most 100 MiB")
	flags.Int64Var(&options.MaxResultBytes, "max-result-bytes", 64<<20, "result byte limit, at most 128 MiB")
	flags.IntVar(&options.MaxJobs, "max-jobs", 128, "retained job limit, at most 128")
	flags.Int64Var(&options.MaxSpoolBytes, "max-spool-bytes", 40<<30, "reserved gateway storage, at most 40 GiB")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("configure conversion worker: %w", err)
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected worker arguments")
	}
	address, err := netip.ParseAddrPort(listen)
	shared := netip.MustParsePrefix("100.64.0.0/10")
	if err != nil || address.Port() == 0 || address.Addr().IsUnspecified() || (!address.Addr().IsLoopback() && !address.Addr().IsPrivate() && !shared.Contains(address.Addr())) {
		return errors.New("worker listener must be an explicit loopback, private, or shared-network IP and port")
	}
	if backendMatchesListener(options.BackendURL, address) {
		return errors.New("backend must differ from worker listener")
	}
	if options.Directory == "" {
		return errors.New("worker directory is required")
	}
	options.Directory, err = filepath.Abs(options.Directory)
	if err != nil {
		return fmt.Errorf("configure conversion worker: %w", err)
	}
	options.APIKey = environment("DOCBANK_WORKER_API_KEY")
	options.BackendKey = environment("DOCBANK_WORKER_BACKEND_KEY")
	worker, err := conversionworker.Open(ctx, options)
	if err != nil {
		return fmt.Errorf("configure conversion worker: %w", err)
	}
	lifetime, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() { retErr = errors.Join(retErr, worker.Close(context.WithoutCancel(ctx))) }()
	listener, err := (&net.ListenConfig{}).Listen(lifetime, "tcp", listen)
	if err != nil {
		return fmt.Errorf("configure conversion worker: %w", err)
	}
	server := &http.Server{Handler: worker, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 2 * time.Minute, WriteTimeout: 2 * time.Minute, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10, BaseContext: func(net.Listener) context.Context { return lifetime }}
	serving := make(chan error, 1)
	running := make(chan error, 1)
	go func() { serving <- server.Serve(listener) }()
	go func() { running <- worker.Run(lifetime) }()
	select {
	case <-ctx.Done():
		retErr = ctx.Err()
	case err := <-serving:
		if !errors.Is(err, http.ErrServerClosed) {
			retErr = err
		}
		serving = nil
	case err := <-running:
		if !errors.Is(err, context.Canceled) {
			retErr = err
		}
		running = nil
	}
	cancel()
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer stop()
	if err := server.Shutdown(cleanup); err != nil {
		retErr = errors.Join(retErr, err, server.Close())
	}
	if serving != nil {
		err := <-serving
		if !errors.Is(err, http.ErrServerClosed) {
			retErr = errors.Join(retErr, err)
		}
	}
	if running != nil {
		err := <-running
		if !errors.Is(err, context.Canceled) {
			retErr = errors.Join(retErr, err)
		}
	}
	return retErr
}

func backendMatchesListener(origin string, listener netip.AddrPort) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "http" {
		return false
	}
	address, err := netip.ParseAddr(parsed.Hostname())
	if err != nil {
		return false
	}
	port := uint64(80)
	if parsed.Port() != "" {
		port, err = strconv.ParseUint(parsed.Port(), 10, 16)
		if err != nil {
			return false
		}
	}
	return address.Unmap().WithZone("") == listener.Addr().Unmap().WithZone("") && uint16(port) == listener.Port()
}
