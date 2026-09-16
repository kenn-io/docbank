package main

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type budgetKey struct {
	path      string
	function  string
	assertion string
	budget    time.Duration
}

type budgetAllowance struct {
	count  int
	reason string
}

var allowedBudgets = map[budgetKey]budgetAllowance{
	{"vault_external_test.go", "TestEmbeddedProcessingWaitsForBackupFreeze", "github.com/stretchr/testify/require.Never", 100 * time.Millisecond}: {1, "Observe processing writes around the real SQLite backup freeze."},
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	if len(args) > 1 {
		_, _ = fmt.Fprintln(stderr, "usage: check-timing-budgets [directory]")
		return 2
	}
	root := "."
	if len(args) == 1 {
		root = args[0]
	}
	violations, err := check(root, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "check-timing-budgets: %v\n", err)
		return 2
	}
	if violations > 0 {
		return 1
	}
	return 0
}

func check(root string, stderr io.Writer) (int, error) {
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("%s: expected a directory", root)
	}
	walkRoot, err := filepath.Abs(root)
	if err != nil {
		return 0, err
	}
	allowanceRoot := findModuleRoot(walkRoot)
	if allowanceRoot == "" {
		allowanceRoot = walkRoot
	}
	used := make(map[budgetKey]int)
	violations := 0
	err = filepath.WalkDir(walkRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != walkRoot && (name == "vendor" || name == "node_modules" || name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(name, "_test.go") {
			return nil
		}
		relativePath, err := filepath.Rel(allowanceRoot, path)
		if err != nil {
			return err
		}
		count, err := checkFile(path, filepath.ToSlash(relativePath), used, stderr)
		violations += count
		return err
	})
	return violations, err
}

func findModuleRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func checkFile(path, relativePath string, used map[budgetKey]int, stderr io.Writer) (int, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	fset := token.NewFileSet()
	// Parser object resolution distinguishes local declarations from import qualifiers.
	file, err := parser.ParseFile(fset, relativePath, source, 0)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", relativePath, err)
	}
	imports := make(map[string]string)
	timeImport := ""
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return 0, fmt.Errorf("unquote import path in %s: %w", relativePath, err)
		}
		name := filepath.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "." || name == "_" {
			continue
		}
		switch importPath {
		case "github.com/stretchr/testify/assert", "github.com/stretchr/testify/require":
			imports[name] = importPath
		case "time":
			timeImport = name
		}
	}
	violations := 0
	for _, decl := range file.Decls {
		function := ""
		if fn, ok := decl.(*ast.FuncDecl); ok {
			function = fn.Name.Name
		}
		ast.Inspect(decl, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) < 4 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (selector.Sel.Name != "Eventually" && selector.Sel.Name != "EventuallyWithT" && selector.Sel.Name != "Never") {
				return true
			}
			qualifier, ok := selector.X.(*ast.Ident)
			if !ok || qualifier.Obj != nil || imports[qualifier.Name] == "" {
				return true
			}
			budget, known := literalDuration(call.Args[2], timeImport)
			if !known || budget >= time.Second {
				return true
			}
			assertion := imports[qualifier.Name] + "." + selector.Sel.Name
			key := budgetKey{relativePath, function, assertion, budget}
			used[key]++
			if used[key] <= allowedBudgets[key].count {
				return true
			}
			violations++
			_, _ = fmt.Fprintf(stderr, "%s:%d: %s budget %s is below 1s; synchronize in-process work or justify a retained integration budget in allowedBudgets\n", relativePath, fset.Position(call.Pos()).Line, assertion, budget)
			return true
		})
	}
	return violations, nil
}

func literalDuration(expr ast.Expr, timeImport string) (time.Duration, bool) {
	expression, ok := literalExpression(expr, timeImport)
	if !ok {
		return 0, false
	}
	timePackage, err := importer.Default().Import("time")
	if err != nil {
		return 0, false
	}
	pkg := types.NewPackage("", "fixture")
	pkg.Scope().Insert(types.NewPkgName(token.NoPos, pkg, "time", timePackage))
	value, err := types.Eval(token.NewFileSet(), pkg, token.NoPos, expression)
	if err != nil || value.Value == nil {
		return 0, false
	}
	nanoseconds, ok := constant.Int64Val(constant.ToInt(value.Value))
	return time.Duration(nanoseconds), ok
}

func literalExpression(expr ast.Expr, timeImport string) (string, bool) {
	switch expr := expr.(type) {
	case *ast.BasicLit:
		return expr.Value, expr.Kind == token.INT || expr.Kind == token.FLOAT
	case *ast.ParenExpr:
		inner, ok := literalExpression(expr.X, timeImport)
		return "(" + inner + ")", ok
	case *ast.UnaryExpr:
		if expr.Op != token.ADD && expr.Op != token.SUB && expr.Op != token.XOR {
			return "", false
		}
		inner, ok := literalExpression(expr.X, timeImport)
		return "(" + expr.Op.String() + inner + ")", ok
	case *ast.BinaryExpr:
		switch expr.Op {
		case token.ADD, token.SUB, token.MUL, token.QUO, token.REM, token.AND, token.OR, token.XOR, token.SHL, token.SHR, token.AND_NOT:
			left, leftOK := literalExpression(expr.X, timeImport)
			right, rightOK := literalExpression(expr.Y, timeImport)
			return "(" + left + " " + expr.Op.String() + " " + right + ")", leftOK && rightOK
		default:
			return "", false
		}
	case *ast.SelectorExpr:
		qualifier, ok := expr.X.(*ast.Ident)
		if !ok || qualifier.Obj != nil || qualifier.Name != timeImport {
			return "", false
		}
		units := map[string]time.Duration{"Nanosecond": time.Nanosecond, "Microsecond": time.Microsecond, "Millisecond": time.Millisecond, "Second": time.Second, "Minute": time.Minute, "Hour": time.Hour}
		_, ok = units[expr.Sel.Name]
		return "time." + expr.Sel.Name, ok
	case *ast.CallExpr:
		selector, ok := expr.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Duration" || len(expr.Args) != 1 || expr.Ellipsis.IsValid() {
			return "", false
		}
		qualifier, ok := selector.X.(*ast.Ident)
		if !ok || qualifier.Obj != nil || qualifier.Name != timeImport {
			return "", false
		}
		inner, ok := literalExpression(expr.Args[0], timeImport)
		return "time.Duration(" + inner + ")", ok
	}
	return "", false
}
