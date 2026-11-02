// Package nohttpmux reports direct net/http route registration in non-test
// code. Repositories that serve their API through a typed router (Huma) want
// every route to carry an operation contract, so plain mux.Handle and
// mux.HandleFunc calls bypass validation, documentation, and error shaping.
package nohttpmux

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// Analyzer reports Handle and HandleFunc registrations on net/http muxes.
var Analyzer = &analysis.Analyzer{
	Name:     "nohttpmux",
	Doc:      "reports direct net/http Handle and HandleFunc route registration outside tests",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

const diagnosticMessage = "direct net/http route registration; register routes through the typed API router so they carry an operation contract"

func run(pass *analysis.Pass) (any, error) {
	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	inspect.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node) {
		call := n.(*ast.CallExpr)
		if strings.HasSuffix(pass.Fset.Position(call.Pos()).Filename, "_test.go") {
			return
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
			return
		}
		fn, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
		if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "net/http" {
			return
		}
		// Package-level http.Handle/HandleFunc register on DefaultServeMux;
		// method calls are only reported for *http.ServeMux receivers.
		if recv := fn.Signature().Recv(); recv != nil {
			if named, ok := types.Unalias(recv.Type()).(*types.Pointer); !ok || named.Elem().String() != "net/http.ServeMux" {
				return
			}
		}
		pass.Reportf(call.Pos(), "%s", diagnosticMessage)
	})

	return nil, nil
}
