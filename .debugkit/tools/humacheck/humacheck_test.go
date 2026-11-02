package humacheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"

	gittest "go.kenn.io/kit/git/test"
)

// loadFixture loads GOPATH-style fixture packages from testdata/src.
func loadFixture(t *testing.T, patterns ...string) []*packages.Package {
	t.Helper()
	dir, err := filepath.Abs("testdata")
	require.NoError(t, err)
	env := append(os.Environ(), "GOPATH="+dir, "GO111MODULE=off", "GOWORK=off", "GOFLAGS=", "GOPROXY=off")
	cfg, deps, err := loadConfig(t.Context(), dir, env)
	require.NoError(t, err)
	pkgs, err := packages.Load(cfg, patterns...)
	require.NoError(t, err)
	require.NoError(t, packageErrors(pkgs, deps))
	return pkgs
}

type expectation struct {
	file    string
	line    int
	pattern *regexp.Regexp
}

var wantPattern = regexp.MustCompile(`// want "((?:[^"\\]|\\.)*)"`)

// expectations parses `// want "regexp"` comments from fixture sources.
func expectations(t *testing.T, pkgs []*packages.Package) []expectation {
	t.Helper()
	var wants []expectation
	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			for _, group := range file.Comments {
				for _, comment := range group.List {
					m := wantPattern.FindStringSubmatch(comment.Text)
					if m == nil {
						continue
					}
					pos := pkg.Fset.Position(comment.Pos())
					wants = append(wants, expectation{
						file:    filepath.Base(pos.Filename),
						line:    pos.Line,
						pattern: regexp.MustCompile(strings.ReplaceAll(m[1], `\"`, `"`)),
					})
				}
			}
		}
	}
	return wants
}

// assertDiagnostics checks that every expectation is met exactly once and
// nothing else was reported.
func assertDiagnostics(t *testing.T, pkgs []*packages.Package, diags []Diagnostic) {
	t.Helper()
	wants := expectations(t, pkgs)
	matched := make([]bool, len(wants))
	for _, d := range diags {
		found := false
		for i, w := range wants {
			if matched[i] || w.file != filepath.Base(d.Path) || w.line != d.Line || !w.pattern.MatchString(d.Message) {
				continue
			}
			matched[i] = true
			found = true
			break
		}
		assert.True(t, found, "unexpected diagnostic %s:%d: %s (%s)", filepath.Base(d.Path), d.Line, d.Message, d.Rule)
	}
	for i, w := range wants {
		assert.True(t, matched[i], "missing diagnostic at %s:%d matching %q", w.file, w.line, w.pattern)
	}
}

// goOnly runs the Go rules over a fixture. The fixture's own files are the
// tracked set (rooted at testdata/src) so the module-wide import ban sees
// them, and its go.mod is the anchor for module-wide findings.
func goOnly(t *testing.T, fixture string, pkgs []*packages.Package) []Diagnostic {
	t.Helper()
	repo := os.DirFS("testdata/src")
	var tracked []string
	require.NoError(t, fs.WalkDir(repo, fixture, func(name string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			tracked = append(tracked, name)
		}
		return err
	}))
	diags, err := check(pkgs, repo, tracked, fixture+"/go.mod", []string{RuleSpec, RuleGenerator, RuleFrontend})
	require.NoError(t, err)
	return diags
}

func TestJSONV2Rule(t *testing.T) {
	t.Parallel()
	for _, fixture := range []string{"v2inline", "v2global", "v2shared", "v1import"} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			pkgs := loadFixture(t, fixture+"/...")
			assertDiagnostics(t, pkgs, goOnly(t, fixture, pkgs))
		})
	}
}

func TestJSONV2MissingInstall(t *testing.T) {
	t.Parallel()
	for _, fixture := range []string{"nov2", "defaultvar", "pkglevel", "unusedmap", "lateoverride", "clearedoverride"} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			pkgs := loadFixture(t, fixture+"/...")
			diags := goOnly(t, fixture, pkgs)
			want := Diagnostic{Path: fixture + "/go.mod", Line: 1, Column: 1, Rule: RuleJSONV2, Message: jsonV2MissingMessage}
			assert.Contains(t, diags, want)
			for _, d := range diags {
				if d != want {
					assert.True(t, strings.HasPrefix(d.Message, "encoding/json (v1) is imported"), "only the fixture's own v1 import may accompany the missing-install finding: %s", d)
				}
			}
		})
	}
}

func TestPackageLevelInitializersCount(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	pkgs := loadFixture(t, "pkglevel")
	prog := newProgram(pkgs)
	assert.Len(prog.constructionSites(), 1, "the adapter call in a package-level initializer")
	routes := collectRoutes(prog)
	assert.Equal([]string{"/api"}, routes.adapterPrefixes)
	assert.Equal([]string{"/v3"}, routes.groupPrefixes)
}

// TestRunUsesStagedGoSources stages a file with a v1 import, then removes the
// import in the working tree only: the diagnostic must still be reported.
func TestRunUsesStagedGoSources(t *testing.T) {
	t.Parallel()
	repo := gittest.NewRepo(t, gittest.Options{ResolvePath: true})
	repo.WriteFile("go.mod", "module m\n\ngo "+goVersion(t)+"\n")
	repo.WriteFile("m.go", "package m\n\nimport \"encoding/json\"\n\nvar _ = json.Marshal\n")
	// Excluded by build tags on every platform, so never loaded; the import
	// ban still sees it because it reads tracked files, not packages.
	repo.WriteFile("m_plan9.go", "//go:build plan9\n\npackage m\n\nimport \"encoding/json\"\n\nvar _ = json.Marshal\n")
	// A nested module owns its own files; its import is not this module's.
	repo.WriteFile("tools/gen/go.mod", "module m/tools/gen\n\ngo "+goVersion(t)+"\n")
	repo.WriteFile("tools/gen/gen.go", "package gen\n\nimport \"encoding/json\"\n\nvar _ = json.Marshal\n")
	repo.Run("add", "-A")
	repo.WriteFile("m.go", "package m\n")

	diags, err := Run(t.Context(), Options{Dir: repo.Root})
	require.NoError(t, err)
	assert.Equal(t, []Diagnostic{
		{Path: "m.go", Line: 3, Column: 8, Rule: RuleJSONV2, Message: jsonV1ImportMessage},
		{Path: "m_plan9.go", Line: 5, Column: 8, Rule: RuleJSONV2, Message: jsonV1ImportMessage},
	}, diags)
}

func TestRewriteJSONV1(t *testing.T) {
	t.Parallel()
	src := `package m

import (
	"encoding/json"
	"io"
	"net/http"
)

func write(w http.ResponseWriter, v any) error { return json.NewEncoder(w).Encode(v) }

func read(r io.Reader, v any) error { return json.NewDecoder(r).Decode(v) }

func pretty(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

func prefixed(v any) ([]byte, error) { return json.MarshalIndent(v, "> ", "\t") }

func plain(v any) ([]byte, error) { return json.Marshal(v) }

func parens(w io.Writer, v any) error { return (json.NewEncoder)(w).Encode(v) }

var raw json.RawMessage // no v2 equivalent; left for the compiler
`
	want := `package m

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
)

func write(w http.ResponseWriter, v any) error { return json.MarshalWrite(w, v) }

func read(r io.Reader, v any) error { return json.UnmarshalRead(r, v) }

func pretty(v any) ([]byte, error) { return json.Marshal(v, jsontext.WithIndent("  ")) }

func prefixed(v any) ([]byte, error) {
	return json.Marshal(v, jsontext.WithIndent("\t"), jsontext.WithIndentPrefix("> "))
}

func plain(v any) ([]byte, error) { return json.Marshal(v) }

func parens(w io.Writer, v any) error { return json.MarshalWrite(w, v) }

var raw json.RawMessage // no v2 equivalent; left for the compiler
`
	assert := assert.New(t)
	require := require.New(t)
	out, changed, err := rewriteJSONV1("m.go", []byte(src))
	require.NoError(err)
	require.True(changed)
	assert.Equal([]byte(want), out)

	_, changed, err = rewriteJSONV1("v2.go", []byte("package m\n\nimport \"encoding/json/v2\"\n\nvar _ = json.Marshal\n"))
	require.NoError(err)
	assert.False(changed, "a file already on v2 is untouched")
}

// TestRunFixesJSONV1Imports: with Fix on, the working-tree file is rewritten
// and reported as fixed; the run still returns the finding so a hook fails
// and the user restages the file.
func TestRunFixesJSONV1Imports(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	repo := gittest.NewRepo(t, gittest.Options{ResolvePath: true})
	repo.WriteFile("go.mod", "module m\n\ngo "+goVersion(t)+"\n")
	repo.WriteFile("m.go", "package m\n\nimport \"encoding/json\"\n\nvar _ = json.Marshal\n")
	repo.Run("add", "-A")

	diags, err := Run(t.Context(), Options{Dir: repo.Root, Fix: true})
	require.NoError(err)
	require.Len(diags, 1)
	assert.Equal(Diagnostic{Path: "m.go", Line: 3, Column: 8, Rule: RuleJSONV2, Message: jsonV1FixedMessage}, diags[0])
	content, err := os.ReadFile(filepath.Join(repo.Root, "m.go"))
	require.NoError(err)
	rewritten, err := parser.ParseFile(token.NewFileSet(), "m.go", content, parser.ImportsOnly)
	require.NoError(err)
	require.Len(rewritten.Imports, 1)
	assert.Equal("encoding/json/v2", importPath(rewritten.Imports[0]))

	// The rewritten file is not staged yet, so a second run judges the old
	// index content; the working tree is already on v2, so nothing is
	// rewritten and the import finding is reported as is.
	diags, err = Run(t.Context(), Options{Dir: repo.Root, Fix: true})
	require.NoError(err)
	require.Len(diags, 1)
	assert.Equal(Diagnostic{Path: "m.go", Line: 3, Column: 8, Rule: RuleJSONV2, Message: jsonV1ImportMessage}, diags[0])

	// A working-tree copy that cannot be fixed keeps the staged finding and
	// does not abort the run.
	require.NoError(os.Remove(filepath.Join(repo.Root, "m.go")))
	diags, err = Run(t.Context(), Options{Dir: repo.Root, Fix: true})
	require.NoError(err)
	require.Len(diags, 1)
	assert.Equal(Diagnostic{Path: "m.go", Line: 3, Column: 8, Rule: RuleJSONV2, Message: jsonV1ImportMessage}, diags[0])
}

// TestRunFailsOnUnparsableTrackedGoFile: a tracked production file the
// import ban cannot parse fails the run rather than passing silently.
func TestRunFailsOnUnparsableTrackedGoFile(t *testing.T) {
	t.Parallel()
	repo := gittest.NewRepo(t, gittest.Options{ResolvePath: true})
	repo.WriteFile("go.mod", "module m\n\ngo "+goVersion(t)+"\n")
	repo.WriteFile("m.go", "package m\n")
	repo.WriteFile("m_plan9.go", "//go:build plan9\n\npackage m\n\nimport (\n\"encoding/json\"\n\nvar _ = json.Marshal\n")
	repo.Run("add", "-A")

	_, err := Run(t.Context(), Options{Dir: repo.Root})
	require.ErrorContains(t, err, "m_plan9.go")
}

// Dependency files load without function bodies, which leaves go/types soft
// errors (unused imports, bodiless init) in the standard library. Those must
// not fail the run, while the same kind of error in a module file still does.
func TestRunReportsModuleSoftErrorsButNotStrippedDependencies(t *testing.T) {
	t.Parallel()
	repo := gittest.NewRepo(t, gittest.Options{ResolvePath: true})
	repo.WriteFile("go.mod", "module m\n\ngo "+goVersion(t)+"\n")
	repo.WriteFile("m.go", "package m\n\nimport \"net/http\"\n\nvar _ = http.MethodGet\n")
	repo.Run("add", "-A")

	diags, err := Run(t.Context(), Options{Dir: repo.Root})
	require.NoError(t, err)
	assert.Empty(t, diags)

	repo.WriteFile("m.go", "package m\n\nimport (\n\t\"net/http\"\n\t\"os\"\n)\n\nvar _ = http.MethodGet\n")
	repo.Run("add", "-A")
	_, err = Run(t.Context(), Options{Dir: repo.Root})
	require.ErrorContains(t, err, `"os" imported and not used`)
}

// goVersion returns the go directive of this module so fixture modules load
// with the same toolchain.
func goVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../go.mod")
	require.NoError(t, err)
	m := regexp.MustCompile(`(?m)^go (\S+)`).FindStringSubmatch(string(data))
	require.NotNil(t, m)
	return m[1]
}

func TestClientRule(t *testing.T) {
	t.Parallel()
	pkgs := loadFixture(t, "routes/...")
	assertDiagnostics(t, pkgs, goOnly(t, "routes", pkgs))
}

func TestCollectRoutes(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	pkgs := loadFixture(t, "routes/server")
	routes := collectRoutes(newProgram(pkgs))
	assert.Equal([]string{"/api/v1"}, routes.adapterPrefixes)
	assert.Equal([]string{"/v2"}, routes.groupPrefixes)
	assert.ElementsMatch([]string{"/ping", "/accounts/{id}", "/jobs", "/jobs/{id}/review", "/projects/{project}/issues/{issue}/actions/close", "/projects/{project}", "/queue", "/raw/{id}", "/grouped"}, routes.paths)
	assert.Equal(receiverGroup, routes.kinds["/grouped"])
	assert.Equal(receiverAPI, routes.kinds["/ping"])
	assert.Equal(receiverAPI, routes.kinds["/raw/{id}"], "the helper is called with a plain API")
	assert.Contains(routes.concrete, "/api/v1/v2/grouped")
	assert.NotContains(routes.concrete, "/api/v1/grouped", "a grouped path is not mounted bare")
	assert.NotContains(routes.concrete, "/api/v1/v2/ping", "an ungrouped path is not mounted under the group")
	assert.Contains(routes.concrete, "/api/v1/raw/{id}")
	assert.NotContains(routes.concrete, "/api/v1/v2/raw/{id}")
	assert.NotContains(routes.concrete, "/v2/api/v1/grouped")
	assert.NotContains(routes.concrete, "/grouped")
}

func TestConstructionSitesSkipTestsAndGenerated(t *testing.T) {
	t.Parallel()
	pkgs := loadFixture(t, "routes/...")
	prog := newProgram(pkgs)
	skipped := 0
	for _, pkg := range prog.pkgs {
		for _, file := range pkg.Syntax {
			if prog.skipFile(file) {
				skipped++
			}
		}
	}
	assert.Equal(t, 1, skipped, "generated client file is skipped; the _test.go file is not loaded")
	sites := prog.constructionSites()
	require.Len(t, sites, 1)
	assert.IsType(t, &ast.CallExpr{}, sites[0].call)
}

func TestRunRejectsUnknownRule(t *testing.T) {
	t.Parallel()
	_, err := Run(t.Context(), Options{Disabled: []string{"bogus"}})
	require.ErrorContains(t, err, `unknown rule "bogus"`)
}
