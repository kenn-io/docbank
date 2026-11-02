// Command kennlint runs the kit Go analyzers and renders the shared
// golangci-lint configuration.
//
//	kennlint run [analysis flags] ./...
//	kennlint workflow-retries [path ...]
//	kennlint sql [path ...]
//	kennlint config [-overlay FILE] [-out FILE] [-check]
//	kennlint profile [-tags LIST] [-disable LIST] [-parallel] [-cpuprofile FILE] [packages]
//	kennlint analyzers
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/tools/go/analysis/multichecker"

	"go.kenn.io/kit/lint"
	"go.kenn.io/kit/lint/config"
	"go.kenn.io/kit/lint/sqlcheck"
)

const usage = `usage:
  kennlint run [analysis flags] [packages]   run the kit analyzers (go vet style)
  kennlint workflow-retries [path ...]       report downloads missing retries in workflow YAML (default: .github)
  kennlint sql [path ...]                    report CHECK constraints and enum types in .sql files (default: .)
  kennlint config [flags]                    render .golangci.yml from the canonical config and an overlay
  kennlint profile [flags] [packages]        time each analyzer, split between packages and dependencies
  kennlint analyzers                         list the analyzers and their documentation
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		// multichecker parses os.Args itself; drop the subcommand.
		os.Args = append(os.Args[:1], os.Args[2:]...)
		multichecker.Main(lint.Analyzers()...)
	case "sql":
		n, err := runSQL(os.Args[2:], os.Stdout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "kennlint sql: %v\n", err)
			os.Exit(2)
		}
		if n > 0 {
			os.Exit(1)
		}
	case "workflow-retries":
		n, err := runWorkflowRetries(os.Args[2:], os.Stdout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "kennlint workflow-retries: %v\n", err)
			os.Exit(2)
		}
		if n > 0 {
			os.Exit(1)
		}
	case "config":
		if err := runConfig(os.Args[2:], os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "kennlint config: %v\n", err)
			os.Exit(1)
		}
	case "profile":
		if err := runProfile(os.Args[2:], os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "kennlint profile: %v\n", err)
			os.Exit(1)
		}
	case "analyzers":
		for _, a := range lint.Analyzers() {
			fmt.Printf("%-14s %s\n", a.Name, a.Doc)
		}
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// errDrift reports that the committed configuration differs from the render.
var errDrift = errors.New("configuration is out of date")

func runConfig(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("kennlint config", flag.ContinueOnError)
	flags.SetOutput(stderr)
	overlayPath := flags.String("overlay", ".golangci.overlay.yml", "repository overlay merged into the canonical config; skipped when absent")
	outPath := flags.String("out", ".golangci.yml", "file to write; - writes to stdout")
	check := flags.Bool("check", false, "exit non-zero when -out differs from the rendered config instead of writing it")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}

	overlay, err := os.ReadFile(*overlayPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading overlay %s: %w", *overlayPath, err)
	}
	rendered, err := config.Render(overlay)
	if err != nil {
		return err
	}

	switch {
	case *outPath == "-":
		_, err := stdout.Write(rendered)
		return err
	case *check:
		current, err := os.ReadFile(*outPath)
		if err != nil {
			return fmt.Errorf("reading %s: %w", *outPath, err)
		}
		if !bytes.Equal(current, rendered) {
			return fmt.Errorf("%s: %w; run kennlint config to regenerate it from %s", *outPath, errDrift, *overlayPath)
		}
		return nil
	default:
		if err := os.WriteFile(*outPath, rendered, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", *outPath, err)
		}
		fmt.Fprintf(stdout, "wrote %s\n", *outPath)
		return nil
	}
}

// runSQL scans .sql files under each path (or the current directory) and
// prints one line per finding. It returns the number of findings.
func runSQL(paths []string, stdout io.Writer) (int, error) {
	if len(paths) == 0 {
		paths = []string{"."}
	}
	return scanFiles(paths, []string{".sql"}, func(path string, src []byte) (int, error) {
		findings := sqlcheck.Scan(string(src))
		for _, f := range findings {
			fmt.Fprintf(stdout, "%s:%d:%d: %s\n", filepath.ToSlash(path), f.Line, f.Column, f.Message())
		}
		return len(findings), nil
	})
}

// scanFiles shares path selection and error handling for non-Go checks.
func scanFiles(paths, extensions []string, scan func(string, []byte) (int, error)) (int, error) {
	total := 0
	for _, root := range paths {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if path != root && (d.Name() == ".git" || d.Name() == "node_modules") {
					return filepath.SkipDir
				}
				return nil
			}
			if !slices.Contains(extensions, strings.ToLower(filepath.Ext(path))) {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			n, err := scan(path, src)
			total += n
			if err != nil {
				return fmt.Errorf("%s: %w", filepath.ToSlash(path), err)
			}
			return nil
		})
		if err != nil {
			return total, fmt.Errorf("scanning %s: %w", root, err)
		}
	}
	return total, nil
}
