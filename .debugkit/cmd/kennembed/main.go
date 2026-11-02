package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"go.kenn.io/kit/embedserver"
)

func main() {
	os.Exit(run(os.Args[1:], embedserver.Serve))
}

func run(args []string, serve func(context.Context, embedserver.Options) error) int {
	flags := flag.NewFlagSet("kennembed", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var opts embedserver.Options
	flags.StringVar(&opts.Listen, "listen", "", "loopback listen address (default 127.0.0.1:0)")
	flags.StringVar(&opts.DataDir, "data-dir", "", "data directory (default embedserver.DefaultDir)")
	flags.StringVar(&opts.Library, "library", "", "ONNX Runtime library path (default beside the executable)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "kennembed: unexpected positional arguments")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := serve(ctx, opts); err != nil {
		return 1
	}
	return 0
}
