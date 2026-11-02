// Command huma-check reports Huma API usage that drifts from the shared
// contract. Run it from a module root, typically as `huma-check ./...`.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"go.kenn.io/kit/tools/humacheck"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("huma-check", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	tags := flags.String("tags", "", "comma-separated build tags")
	disable := flags.String("disable", "", "comma-separated rules to skip ("+strings.Join(humacheck.Rules, ", ")+")")
	fix := flags.Bool("fix", true, "rewrite encoding/json (v1) imports to encoding/json/v2 in place; remaining compile errors are left for a manual pass")
	flags.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: huma-check [flags] [packages]\n\n")
		fmt.Fprintf(os.Stderr, "Checks that Huma APIs use encoding/json/v2, commit their OpenAPI document as YAML,\n")
		fmt.Fprintf(os.Stderr, "use a supported client generator, and never hand-roll requests to their own routes.\n")
		fmt.Fprintf(os.Stderr, "Findings with a mechanical fix are rewritten in place unless -fix=false; the run still exits 1 so the files get restaged.\n\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	opts := humacheck.Options{Patterns: flags.Args(), Fix: *fix}
	if *tags != "" {
		opts.BuildTags = strings.Split(*tags, ",")
	}
	if *disable != "" {
		opts.Disabled = strings.Split(*disable, ",")
	}
	diags, err := humacheck.Run(ctx, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "huma-check: %v\n", err)
		return 2
	}
	for _, d := range diags {
		fmt.Println(d.String())
	}
	if len(diags) > 0 {
		return 1
	}
	return 0
}
