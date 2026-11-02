package testifyhelper

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
)

func testifyName(path string) string {
	switch path {
	case "github.com/stretchr/testify/assert":
		return "assert"
	case "github.com/stretchr/testify/require":
		return "require"
	default:
		return ""
	}
}

// checkNames resolves objects so unrelated variables named req or asrt are ignored.
func checkNames(pass *analysis.Pass, file *ast.File) {
	for _, spec := range file.Imports {
		if spec.Name == nil {
			continue
		}
		pkg, ok := pass.TypesInfo.Defs[spec.Name].(*types.PkgName)
		if !ok {
			continue
		}
		name := testifyName(pkg.Imported().Path())
		if name == "" || spec.Name.Name == name {
			continue
		}
		edits := renameObject(pass, file, pkg, name)
		pass.Report(analysis.Diagnostic{Pos: spec.Name.Pos(), Message: "testify import must be named " + name, SuggestedFixes: []analysis.SuggestedFix{{Message: "Use canonical testify import name", TextEdits: edits}}})
	}
	ast.Inspect(file, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		obj, ok := pass.TypesInfo.Defs[ident].(*types.Var)
		if !ok {
			return true
		}
		ptr, ok := obj.Type().(*types.Pointer)
		if !ok {
			return true
		}
		named, ok := ptr.Elem().(*types.Named)
		if !ok || named.Obj().Pkg() == nil || named.Obj().Name() != "Assertions" {
			return true
		}
		name := testifyName(named.Obj().Pkg().Path())
		if name == "" || ident.Name == name {
			return true
		}
		edits, needsExpansion := expandHelper(pass, file, obj, name)
		if !needsExpansion {
			edits = renameObject(pass, file, obj, name)
		}
		diagnostic := analysis.Diagnostic{Pos: ident.Pos(), Message: "testify assertion object must be named " + name}
		if len(edits) > 0 {
			diagnostic.SuggestedFixes = []analysis.SuggestedFix{{Message: "Use canonical testify helper name", TextEdits: edits}}
		}
		pass.Report(diagnostic)
		return true
	})
}

func renameObject(pass *analysis.Pass, file *ast.File, obj types.Object, name string) []analysis.TextEdit {
	var edits []analysis.TextEdit
	ast.Inspect(file, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if ok && identObject(pass, ident) == obj {
			edits = append(edits, analysis.TextEdit{Pos: ident.Pos(), End: ident.End(), NewText: []byte(name)})
		}
		return true
	})
	return edits
}

// packageNeeded keeps package constants, types, constructors, and calls in
// nested functions accessible when introducing or renaming a local helper.
func packageNeeded(pass *analysis.Pass, body *ast.BlockStmt, name string, allowed []ast.Node) bool {
	allowedPackages := map[*ast.Ident]bool{}
	for _, node := range allowed {
		if call, ok := node.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if ident, ok := sel.X.(*ast.Ident); ok {
					allowedPackages[ident] = true
				}
			}
		}
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok && !allowedPackages[ident] {
			if pkg, ok := pass.TypesInfo.Uses[ident].(*types.PkgName); ok && testifyName(pkg.Imported().Path()) == name {
				found = true
			}
		}
		return !found
	})
	return found
}

func reportHelper(pass *analysis.Pass, body *ast.BlockStmt, tName, name string, nodes []ast.Node, total int, message string) {
	diagnostic := analysis.Diagnostic{Pos: nodes[len(nodes)-1].Pos(), Message: fmt.Sprintf(message, total)}
	var edits []analysis.TextEdit
	first := nodes[0].Pos()
	insertion := token.NoPos
	for _, stmt := range body.List {
		if stmt.Pos() <= first && first <= stmt.End() {
			insertion = stmt.Pos()
			break
		}
	}
	// Package aliases are fixed separately; only offer helper edits once the
	// package already has its canonical name, avoiding overlapping renames.
	for _, node := range nodes {
		call := node.(*ast.CallExpr)
		sel := call.Fun.(*ast.SelectorExpr)
		pkg := sel.X.(*ast.Ident)
		if pkg.Name != name || len(call.Args) == 0 {
			insertion = token.NoPos
			break
		}
		arg, ok := call.Args[0].(*ast.Ident)
		if !ok || arg.Name != tName {
			insertion = token.NoPos
			break
		}
		end := call.Args[0].End()
		if len(call.Args) > 1 {
			end = call.Args[1].Pos()
		}
		edits = append(edits, analysis.TextEdit{Pos: call.Args[0].Pos(), End: end})
	}
	if insertion.IsValid() {
		edits = append(edits, analysis.TextEdit{Pos: insertion, End: insertion, NewText: []byte(name + " := " + name + ".New(" + tName + ")\n")})
		diagnostic.SuggestedFixes = []analysis.SuggestedFix{{Message: "Create a local " + name + " helper", TextEdits: edits}}
	}
	pass.Report(diagnostic)
}

// expandHelper preserves package access by returning this helper's method
// calls to package calls with their original test argument.
func expandHelper(pass *analysis.Pass, file *ast.File, obj types.Object, name string) ([]analysis.TextEdit, bool) {
	var declaration *ast.AssignStmt
	var body *ast.BlockStmt
	ast.Inspect(file, func(n ast.Node) bool {
		var candidate *ast.BlockStmt
		switch fn := n.(type) {
		case *ast.FuncDecl:
			candidate = fn.Body
		case *ast.FuncLit:
			candidate = fn.Body
		}
		if candidate != nil && candidate.Pos() <= obj.Pos() && obj.Pos() < candidate.End() {
			body = candidate
		}
		if stmt, ok := n.(*ast.AssignStmt); ok && len(stmt.Lhs) == 1 && len(stmt.Rhs) == 1 {
			if ident, ok := stmt.Lhs[0].(*ast.Ident); ok && identObject(pass, ident) == obj {
				declaration = stmt
			}
		}
		return true
	})
	if declaration == nil || body == nil || !packageNeeded(pass, body, name, []ast.Node{declaration.Rhs[0]}) {
		return nil, false
	}
	call, ok := declaration.Rhs[0].(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return nil, true
	}
	constructor, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || constructor.Sel.Name != "New" {
		return nil, true
	}
	pkg, ok := constructor.X.(*ast.Ident)
	if !ok {
		return nil, true
	}
	pkgObj, ok := pass.TypesInfo.Uses[pkg].(*types.PkgName)
	if !ok || testifyName(pkgObj.Imported().Path()) != name {
		return nil, true
	}
	arg, ok := call.Args[0].(*ast.Ident)
	if !ok {
		return nil, true
	}
	edits := []analysis.TextEdit{{Pos: declaration.Pos(), End: declaration.End()}}
	handled := map[*ast.Ident]bool{}
	argumentVisible := true
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || identObject(pass, ident) != obj {
			return true
		}
		for _, scope := range pass.TypesInfo.Scopes {
			if scope.Contains(call.Pos()) {
				_, resolved := scope.LookupParent(arg.Name, call.Pos())
				if resolved != nil && resolved != identObject(pass, arg) {
					argumentVisible = false
				}
			}
		}
		handled[ident] = true
		edits = append(edits, analysis.TextEdit{Pos: ident.Pos(), End: ident.End(), NewText: []byte(pkg.Name)})
		text := arg.Name
		if len(call.Args) > 0 {
			text += ", "
		}
		edits = append(edits, analysis.TextEdit{Pos: call.Lparen + 1, End: call.Lparen + 1, NewText: []byte(text)})
		return true
	})
	if !argumentVisible {
		return nil, true
	}
	for ident, used := range pass.TypesInfo.Uses {
		if used == obj && !handled[ident] {
			return nil, true
		}
	}
	return edits, true
}
