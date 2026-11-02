package humacheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

// dependencyFiles recognizes source files that belong to the standard library
// or the module cache. No rule reads syntax outside the main module, so those
// files are parsed without function bodies: go/packages type-checks every
// dependency from source under NeedDeps, and declarations alone give the
// same package types at a fraction of the cost. Anything else (main-module
// files, workspace modules, local replacements, vendored copies) keeps its
// bodies.
type dependencyFiles struct {
	roots []string
}

// newDependencyFiles asks the go command that go/packages will run in dir
// for GOROOT and GOMODCACHE, so a toolchain switch picks the same roots.
func newDependencyFiles(ctx context.Context, dir string, env []string) (*dependencyFiles, error) {
	cmd := exec.CommandContext(ctx, "go", "env", "GOROOT", "GOMODCACHE")
	cmd.Dir = dir
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go env GOROOT GOMODCACHE: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	d := &dependencyFiles{}
	for line := range strings.Lines(string(out)) {
		root := strings.TrimSpace(line)
		if root == "" {
			continue
		}
		d.roots = append(d.roots, root)
		if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root {
			d.roots = append(d.roots, resolved)
		}
	}
	return d, nil
}

func (d *dependencyFiles) contains(filename string) bool {
	for _, root := range d.roots {
		if strings.HasPrefix(filename, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// parseFile is a packages.Config.ParseFile that parses like the go/packages
// default and then drops function bodies from dependency files. Function
// literals in package-level initializers are kept; they are rare and their
// types can matter.
func (d *dependencyFiles) parseFile(fset *token.FileSet, filename string, src []byte) (*ast.File, error) {
	file, err := parser.ParseFile(fset, filename, src, parser.AllErrors|parser.ParseComments)
	if file != nil && d.contains(filename) {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				fn.Body = nil
			}
		}
	}
	return file, err
}

// packageErrors joins the load errors of every package in the graph.
//
// Soft type errors located in dependency files are dropped: removing bodies
// removes the only uses of some imports ("imported and not used") and leaves
// init and generic functions bodiless, which go/types reports as soft errors
// that keep the package's types complete. Hard errors, parse errors, and every
// error in files that keep their bodies still fail the load.
func packageErrors(pkgs []*packages.Package, deps *dependencyFiles) error {
	var errs []error
	packages.Visit(pkgs, nil, func(pkg *packages.Package) {
		stripped := map[string]bool{}
		for _, e := range pkg.TypeErrors {
			if pos := e.Fset.Position(e.Pos); e.Soft && deps.contains(pos.Filename) {
				stripped[pos.String()+"\x00"+e.Msg] = true
			}
		}
		var kept []error
		var skew error
		for _, e := range pkg.Errors {
			switch {
			case e.Kind == packages.TypeError && stripped[e.Pos+"\x00"+e.Msg]:
			case e.Kind == packages.UnknownError && strings.HasPrefix(e.Msg, "This application uses version go1."):
				// go/packages adds this notice to any package with errors
				// when the go command is newer than this binary; it only
				// matters next to an error that is kept.
				skew = errors.New(e.Error())
			default:
				kept = append(kept, errors.New(e.Error()))
			}
		}
		if len(kept) > 0 && skew != nil {
			kept = append(kept, skew)
		}
		errs = append(errs, kept...)
	})
	if len(errs) == 0 {
		return nil
	}
	if len(errs) > 20 {
		errs = append(errs[:20], fmt.Errorf("%d more errors", len(errs)-20))
	}
	return fmt.Errorf("packages failed to load: %w", errors.Join(errs...))
}

// loadConfig returns the packages.Config every load shares.
func loadConfig(ctx context.Context, dir string, env []string) (*packages.Config, *dependencyFiles, error) {
	if env == nil {
		env = os.Environ()
	}
	deps, err := newDependencyFiles(ctx, dir, env)
	if err != nil {
		return nil, nil, err
	}
	return &packages.Config{
		Context:   ctx,
		Dir:       dir,
		Env:       env,
		Mode:      loadMode,
		Tests:     false,
		ParseFile: deps.parseFile,
	}, deps, nil
}
