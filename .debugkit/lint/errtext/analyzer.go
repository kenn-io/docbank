// Package errtext reports code that matches on the text of an error instead of
// its identity. Error strings are not a contract: they change with wrapping and
// wording, so callers should use errors.Is with a sentinel or errors.AsType
// with a typed error.
package errtext

import (
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// Analyzer reports err.Error() text matching.
var Analyzer = &analysis.Analyzer{
	Name:     "errtext",
	Doc:      "reports code that matches on err.Error() text instead of using errors.Is or errors.AsType",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

// IncludeTests controls whether _test.go files are inspected. Tests often
// assert on messages deliberately, so they are skipped by default.
var IncludeTests bool

func init() {
	Analyzer.Flags.BoolVar(&IncludeTests, "include-tests", false, "also report err.Error() matching in _test.go files")
}

const diagnosticMessage = "matching on err.Error() text; compare with errors.Is against a sentinel or extract a typed error with errors.AsType"

// stringMatchers lists the strings package functions that turn an error
// message into a boolean decision.
var stringMatchers = map[string]bool{
	"Contains":    true,
	"ContainsAny": true,
	"HasPrefix":   true,
	"HasSuffix":   true,
	"EqualFold":   true,
	"Index":       true,
	"LastIndex":   true,
}

func run(pass *analysis.Pass) (any, error) {
	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	errorType := types.Universe.Lookup("error").Type().Underlying().(*types.Interface)

	isErrorText := func(expr ast.Expr) bool {
		call, ok := ast.Unparen(expr).(*ast.CallExpr)
		if !ok || len(call.Args) != 0 {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Error" {
			return false
		}
		recv := pass.TypesInfo.TypeOf(sel.X)
		return recv != nil && types.Implements(recv, errorType)
	}

	inspect.Preorder([]ast.Node{(*ast.BinaryExpr)(nil), (*ast.CallExpr)(nil)}, func(n ast.Node) {
		if !IncludeTests && strings.HasSuffix(pass.Fset.Position(n.Pos()).Filename, "_test.go") {
			return
		}
		switch node := n.(type) {
		case *ast.BinaryExpr:
			if node.Op != token.EQL && node.Op != token.NEQ {
				return
			}
			if isErrorText(node.X) || isErrorText(node.Y) {
				pass.Reportf(node.Pos(), "%s", diagnosticMessage)
			}
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok || !stringMatchers[sel.Sel.Name] {
				return
			}
			fn, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
			if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "strings" {
				return
			}
			if slices.ContainsFunc(node.Args, isErrorText) {
				pass.Reportf(node.Pos(), "%s", diagnosticMessage)
			}
		}
	})

	return nil, nil
}
