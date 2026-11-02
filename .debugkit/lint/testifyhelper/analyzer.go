package testifyhelper

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// Analyzer enforces canonical testify names. Local helpers are optional.
var Analyzer = New(false)

// New creates an analyzer with optional local-helper requirements for both
// assert and require. Canonical import and assertion-object names are always checked.
func New(requireHelpers bool) *analysis.Analyzer {
	a := &analysis.Analyzer{
		Name: "testifyhelper",
		Doc:  "enforces canonical testify names and optionally requires local assertion helpers",
		Run:  func(pass *analysis.Pass) (any, error) { return run(pass, requireHelpers) },
	}
	a.Flags.BoolVar(&requireHelpers, "require-helpers", requireHelpers, "require local assert and require helpers for repeated package calls")
	return a
}

const (
	assertDiagnosticMessage  = "test has %d direct testify package calls; create a local assert helper with assert := assert.New(t) and use it for repeated checks"
	requireDiagnosticMessage = "test has %d direct testify package calls; create a local require helper with require := require.New(t) and use it for repeated checks"
)

func run(pass *analysis.Pass, requireHelpers bool) (any, error) {
	for _, file := range pass.Files {
		checkNames(pass, file)
		if !requireHelpers || !strings.HasSuffix(pass.Fset.Position(file.Pos()).Filename, "_test.go") {
			continue
		}

		imports := importNames(file)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !isTestFuncDecl(fn, imports.testing) {
				continue
			}
			analyzeBody(pass, fn.Body, fn.Type.Params.List[0].Names[0].Name, imports)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.FuncLit)
			if !ok || !isTestLikeFuncType(lit.Type, imports.testing) {
				return true
			}
			paramName := lit.Type.Params.List[0].Names[0].Name
			analyzeBody(pass, lit.Body, paramName, imports)
			return true
		})
	}

	return nil, nil
}

type importSet struct {
	testing map[string]struct{}
}

func importNames(file *ast.File) importSet {
	set := importSet{
		testing: make(map[string]struct{}),
	}

	for _, spec := range file.Imports {
		path := strings.Trim(spec.Path.Value, "\"")
		name := ""
		if spec.Name != nil {
			name = spec.Name.Name
		}
		switch path {
		case "testing":
			if name == "" {
				name = "testing"
			}
			set.testing[name] = struct{}{}
		}
	}

	return set
}

func isTestFuncDecl(fn *ast.FuncDecl, testingImports map[string]struct{}) bool {
	if fn.Body == nil || fn.Name == nil || !strings.HasPrefix(fn.Name.Name, "Test") {
		return false
	}
	return isTestLikeFuncType(fn.Type, testingImports)
}

func isTestLikeFuncType(fnType *ast.FuncType, testingImports map[string]struct{}) bool {
	if fnType == nil || fnType.Params == nil || len(fnType.Params.List) != 1 {
		return false
	}
	field := fnType.Params.List[0]
	if len(field.Names) != 1 {
		return false
	}
	return isTestingT(field.Type, testingImports)
}

func isTestingT(expr ast.Expr, testingImports map[string]struct{}) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil || sel.Sel.Name != "T" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	_, ok = testingImports[pkg.Name]
	return ok
}

func analyzeBody(pass *analysis.Pass, body *ast.BlockStmt, tName string, imports importSet) {
	if body == nil {
		return
	}

	var assertHelper, requireHelper helperState
	var assertCallPositions []ast.Node
	var requireCallPositions []ast.Node

	var visit func(ast.Node)
	visit = func(n ast.Node) {
		switch node := n.(type) {
		case *ast.FuncLit:
			return
		case *ast.AssignStmt:
			for i, rhs := range node.Rhs {
				if i >= len(node.Lhs) {
					continue
				}
				if ident, ok := node.Lhs[i].(*ast.Ident); ok {
					if isAssertNewCall(pass, rhs, tName) {
						assertHelper.add(identObject(pass, ident))
					}
					if isRequireNewCall(pass, rhs, tName) {
						requireHelper.add(identObject(pass, ident))
					}
				}
			}
		case *ast.ValueSpec:
			for i, value := range node.Values {
				if i >= len(node.Names) {
					continue
				}
				if isAssertNewCall(pass, value, tName) {
					assertHelper.add(identObject(pass, node.Names[i]))
				}
				if isRequireNewCall(pass, value, tName) {
					requireHelper.add(identObject(pass, node.Names[i]))
				}
			}
		case *ast.CallExpr:
			switch callKind(pass, node, assertHelper, requireHelper) {
			case "assert":
				assertCallPositions = append(assertCallPositions, node)
			case "require":
				requireCallPositions = append(requireCallPositions, node)
			case "assert-helper":
				assertHelper.used = true
			case "require-helper":
				requireHelper.used = true
			}
		}

		ast.Inspect(n, func(child ast.Node) bool {
			if child == nil || child == n {
				return true
			}
			if _, ok := child.(*ast.FuncLit); ok {
				return false
			}
			visit(child)
			return false
		})
	}

	for _, stmt := range body.List {
		visit(stmt)
	}

	total := len(assertCallPositions) + len(requireCallPositions)
	if total <= 3 {
		return
	}

	hasAssertHelper := len(assertHelper.objs) > 0 && assertHelper.used
	hasRequireHelper := len(requireHelper.objs) > 0 && requireHelper.used

	if len(assertCallPositions) >= 2 && !hasAssertHelper && !packageNeeded(pass, body, "assert", assertCallPositions) {
		reportHelper(pass, body, tName, "assert", assertCallPositions, total, assertDiagnosticMessage)
	}

	if len(requireCallPositions) >= 2 && !hasRequireHelper && !packageNeeded(pass, body, "require", requireCallPositions) {
		reportHelper(pass, body, tName, "require", requireCallPositions, total, requireDiagnosticMessage)
	}
}

type helperState struct {
	objs map[types.Object]struct{}
	used bool
}

func (s *helperState) add(obj types.Object) {
	if obj == nil {
		return
	}
	if s.objs == nil {
		s.objs = make(map[types.Object]struct{})
	}
	s.objs[obj] = struct{}{}
}

func (s *helperState) has(obj types.Object) bool {
	if obj == nil {
		return false
	}
	_, ok := s.objs[obj]
	return ok
}

func identObject(pass *analysis.Pass, ident *ast.Ident) types.Object {
	if ident == nil {
		return nil
	}
	if obj := pass.TypesInfo.Defs[ident]; obj != nil {
		return obj
	}
	return pass.TypesInfo.Uses[ident]
}

func isAssertNewCall(pass *analysis.Pass, expr ast.Expr, tName string) bool {
	return isHelperNewCall(pass, expr, tName, "github.com/stretchr/testify/assert")
}

func isRequireNewCall(pass *analysis.Pass, expr ast.Expr, tName string) bool {
	return isHelperNewCall(pass, expr, tName, "github.com/stretchr/testify/require")
}

func isHelperNewCall(pass *analysis.Pass, expr ast.Expr, tName string, importPath string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil || sel.Sel.Name != "New" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	pkgObj, ok := pass.TypesInfo.Uses[pkg].(*types.PkgName)
	if !ok || pkgObj.Imported().Path() != importPath {
		return false
	}
	arg, ok := call.Args[0].(*ast.Ident)
	return ok && arg.Name == tName
}

func callKind(pass *analysis.Pass, call *ast.CallExpr, assertHelper, requireHelper helperState) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	obj := pass.TypesInfo.Uses[pkg]
	switch {
	case assertHelper.has(obj):
		return "assert-helper"
	case requireHelper.has(obj):
		return "require-helper"
	}

	pkgObj, ok := obj.(*types.PkgName)
	if !ok || sel.Sel.Name == "New" || !hasMatchingHelperMethod(pass, sel) {
		return ""
	}

	switch pkgObj.Imported().Path() {
	case "github.com/stretchr/testify/assert":
		return "assert"
	case "github.com/stretchr/testify/require":
		return "require"
	default:
		return ""
	}
}

// hasMatchingHelperMethod proves that removing the testing argument preserves
// the function's remaining parameter and result types.
func hasMatchingHelperMethod(pass *analysis.Pass, selector *ast.SelectorExpr) bool {
	function, ok := pass.TypesInfo.Uses[selector.Sel].(*types.Func)
	if !ok || function.Pkg() == nil {
		return false
	}
	assertions, ok := function.Pkg().Scope().Lookup("Assertions").(*types.TypeName)
	if !ok {
		return false
	}
	method, _, _ := types.LookupFieldOrMethod(types.NewPointer(assertions.Type()), true, function.Pkg(), function.Name())
	if method == nil {
		return false
	}
	functionType, ok := function.Type().(*types.Signature)
	if !ok {
		return false
	}
	methodType, ok := method.Type().(*types.Signature)
	if !ok || functionType.Params().Len() != methodType.Params().Len()+1 || functionType.Variadic() != methodType.Variadic() || !types.Identical(functionType.Results(), methodType.Results()) {
		return false
	}
	for i := range methodType.Params().Len() {
		if !types.Identical(functionType.Params().At(i+1).Type(), methodType.Params().At(i).Type()) {
			return false
		}
	}
	return true
}
