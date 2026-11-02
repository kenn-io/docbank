// Package humacheck reports Huma API usage that drifts from the shared
// contract: APIs must serialize with encoding/json/v2, the generated OpenAPI
// document must be committed as YAML, clients must come from the standard
// generator, and nobody may hand-roll HTTP calls against the module's own
// routes.
package humacheck

import (
	"context"
	"fmt"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"

	"go.kenn.io/kit/pathresolve"
)

// Rule names accepted by Options.Disabled.
const (
	RuleJSONV2    = "jsonv2"
	RuleSpec      = "spec"
	RuleGenerator = "generator"
	RuleClient    = "client"
	RuleFrontend  = "frontend"
)

// Rules lists every rule name in output order.
var Rules = []string{RuleJSONV2, RuleSpec, RuleGenerator, RuleClient, RuleFrontend}

// Options configures a Run.
type Options struct {
	// Dir is the working directory for package loading and path display.
	// Empty means the process working directory.
	Dir string
	// Patterns are go/packages patterns; empty means "./...".
	Patterns []string
	// BuildTags are extra build tags passed to the loader.
	BuildTags []string
	// Disabled lists rule names that must not run.
	Disabled []string
	// Fix rewrites files in the working tree where a finding has a
	// mechanical fix: today, encoding/json (v1) imports become
	// encoding/json/v2 together with the call sites that map one-to-one.
	// Rewritten files are still reported (as fixed) so a hook run exits
	// non-zero and the user restages them.
	Fix bool
}

// Diagnostic is one finding.
type Diagnostic struct {
	Path    string
	Line    int
	Column  int
	Rule    string
	Message string
}

// String renders the diagnostic in the conventional path:line:col form.
func (d Diagnostic) String() string {
	return fmt.Sprintf("%s:%d:%d: %s (%s)", d.Path, d.Line, d.Column, d.Message, d.Rule)
}

// Compare orders diagnostics by path, line, column, then rule.
func (d Diagnostic) Compare(other Diagnostic) int {
	if c := strings.Compare(d.Path, other.Path); c != 0 {
		return c
	}
	if d.Line != other.Line {
		return d.Line - other.Line
	}
	if d.Column != other.Column {
		return d.Column - other.Column
	}
	return strings.Compare(d.Rule, other.Rule)
}

// Run loads the packages named by opts and returns every finding sorted by
// position. Paths are relative to opts.Dir when possible.
//
// Inside a Git repository every rule judges the index: repository files are
// read from it, and tracked Go sources whose working-tree copy differs from
// the index are loaded from the index through a go/packages overlay. Files
// that are not tracked at all still come from the working tree, and a file
// removed from the index but left on disk is still loaded; both are outside
// what an overlay can express.
func Run(ctx context.Context, opts Options) ([]Diagnostic, error) {
	dir := opts.Dir
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		dir = wd
	}
	if resolved, err := pathresolve.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	for _, rule := range opts.Disabled {
		if !slices.Contains(Rules, rule) {
			return nil, fmt.Errorf("unknown rule %q; known rules: %s", rule, strings.Join(Rules, ", "))
		}
	}
	patterns := opts.Patterns
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}

	cfg, deps, err := loadConfig(ctx, dir, nil)
	if err != nil {
		return nil, err
	}
	if len(opts.BuildTags) > 0 {
		cfg.BuildFlags = []string{"-tags=" + strings.Join(opts.BuildTags, ",")}
	}

	root, inGit, err := gitTopLevel(ctx, dir)
	if err != nil {
		return nil, err
	}
	var repo fs.FS
	var tracked []string
	if inGit {
		if resolved, err := pathresolve.EvalSymlinks(root); err == nil {
			root = resolved
		}
		tracked, err = trackedFiles(ctx, root)
		if err != nil {
			return nil, err
		}
		index, err := newIndexFS(ctx, root)
		if err != nil {
			return nil, err
		}
		defer func() { _ = index.Close() }()
		repo = index
		cfg.Overlay, err = stagedOverlay(ctx, root, index)
		if err != nil {
			return nil, err
		}
	}

	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("load packages: %w", err)
	}
	if err := packageErrors(pkgs, deps); err != nil {
		return nil, err
	}
	moduleDir := mainModuleDir(dir, pkgs)
	if !inGit {
		root = moduleDir
		tracked, err = walkFiles(root)
		if err != nil {
			return nil, err
		}
		repo = os.DirFS(root)
	}
	goMod := "go.mod"
	if rel, err := filepath.Rel(root, filepath.Join(moduleDir, "go.mod")); err == nil && !strings.HasPrefix(rel, "..") {
		goMod = filepath.ToSlash(rel)
	}

	diags, err := check(pkgs, repo, tracked, goMod, opts.Disabled)
	if err != nil {
		return nil, err
	}
	if opts.Fix {
		diags = applyJSONFixes(root, diags)
	}
	for i := range diags {
		diags[i].Path = displayPath(dir, root, diags[i].Path)
	}
	slices.SortFunc(diags, Diagnostic.Compare)
	return diags, nil
}

const loadMode = packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
	packages.NeedImports | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax |
	packages.NeedModule | packages.NeedDeps

// check runs every enabled rule over already-loaded packages. Repository rules
// read tracked files (repo-root-relative, forward slashes) through repo.
// goModPath is the repo-relative go.mod that anchors module-wide findings and
// scopes the spec-presence rule.
func check(pkgs []*packages.Package, repo fs.FS, tracked []string, goModPath string, disabled []string) ([]Diagnostic, error) {
	enabled := func(rule string) bool { return !slices.Contains(disabled, rule) }
	prog := newProgram(pkgs)

	var diags []Diagnostic
	hasAPI := len(prog.constructionSites()) > 0
	if enabled(RuleJSONV2) {
		found, err := checkJSONV2(prog, repo, tracked, hasAPI, goModPath)
		if err != nil {
			return nil, err
		}
		diags = append(diags, found...)
	}
	routes := collectRoutes(prog)
	if enabled(RuleClient) {
		diags = append(diags, checkClients(prog, routes)...)
	}
	if enabled(RuleSpec) {
		found, err := checkSpecs(repo, tracked, hasAPI, goModPath)
		if err != nil {
			return nil, err
		}
		diags = append(diags, found...)
	}
	if enabled(RuleGenerator) {
		found, err := checkGenerators(repo, tracked, hasAPI, goModPath)
		if err != nil {
			return nil, err
		}
		diags = append(diags, found...)
	}
	if enabled(RuleFrontend) {
		found, err := checkFrontend(repo, tracked, routes)
		if err != nil {
			return nil, err
		}
		diags = append(diags, found...)
	}
	return diags, nil
}

// mainModuleDir returns the directory of the main module the loaded
// packages belong to, falling back to dir.
func mainModuleDir(dir string, pkgs []*packages.Package) string {
	moduleDir := dir
	for _, pkg := range pkgs {
		if pkg.Module != nil && pkg.Module.Main && pkg.Module.Dir != "" {
			moduleDir = pkg.Module.Dir
			break
		}
	}
	if resolved, err := pathresolve.EvalSymlinks(moduleDir); err == nil {
		moduleDir = resolved
	}
	return moduleDir
}

// stagedOverlay maps every tracked Go source or module file whose
// working-tree copy differs from the index to its index content, so
// go/packages sees what will be committed.
func stagedOverlay(ctx context.Context, root string, index fs.FS) (map[string][]byte, error) {
	changed, err := unstagedChanges(ctx, root)
	if err != nil {
		return nil, err
	}
	overlay := map[string][]byte{}
	for _, name := range changed {
		switch path.Ext(name) {
		case ".go", ".mod", ".sum", ".work":
		default:
			continue
		}
		content, err := fs.ReadFile(index, name)
		if err != nil {
			return nil, fmt.Errorf("read staged %s: %w", name, err)
		}
		overlay[filepath.Join(root, filepath.FromSlash(name))] = content
	}
	return overlay, nil
}

// joinRepoPath turns a repo-relative, forward-slash path into an absolute
// working-tree path.
func joinRepoPath(root, name string) string {
	return filepath.Join(root, filepath.FromSlash(name))
}

// displayPath makes p relative to dir. Repo-relative paths (no separator
// prefix) are first anchored at repoRoot.
func displayPath(dir, repoRoot, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(repoRoot, filepath.FromSlash(p))
	}
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return p
	}
	return rel
}

func positionOf(fset *token.FileSet, pos token.Pos) (string, int, int) {
	position := fset.Position(pos)
	return position.Filename, position.Line, position.Column
}
