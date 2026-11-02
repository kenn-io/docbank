package humacheck

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

const (
	humaPath     = "github.com/danielgtaylor/huma/v2"
	jsonV1Path   = "encoding/json"
	jsonV2Path   = "encoding/json/v2"
	jsonTextPath = "encoding/json/jsontext"
)

// program indexes the loaded packages so rules can follow calls into
// functions declared anywhere in the main module, even when the caller only
// asked for a subset of packages.
type program struct {
	fset  *token.FileSet
	pkgs  []*packages.Package
	funcs map[*types.Func]*funcDecl
}

type funcDecl struct {
	pkg  *packages.Package
	decl *ast.FuncDecl
}

func newProgram(roots []*packages.Package) *program {
	p := &program{funcs: make(map[*types.Func]*funcDecl)}
	if len(roots) > 0 {
		p.fset = roots[0].Fset
	}
	seen := map[*packages.Package]bool{}
	add := func(pkg *packages.Package) {
		if seen[pkg] || pkg.TypesInfo == nil {
			return
		}
		seen[pkg] = true
		p.pkgs = append(p.pkgs, pkg)
	}
	for _, pkg := range roots {
		add(pkg)
	}
	// Dependencies inside the main module are part of the program too: a
	// helper package the roots import still declares routes and wrappers.
	packages.Visit(roots, nil, func(pkg *packages.Package) {
		if pkg.Module != nil && pkg.Module.Main {
			add(pkg)
		}
	})
	for _, pkg := range p.pkgs {
		for _, file := range pkg.Syntax {
			// Generated code is never judged and never followed: a generated
			// client's request helpers must not turn its callers into
			// hand-rolled clients.
			if p.skipFile(file) {
				continue
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				if obj, ok := pkg.TypesInfo.Defs[fn.Name].(*types.Func); ok {
					p.funcs[obj] = &funcDecl{pkg: pkg, decl: fn}
				}
			}
		}
	}
	return p
}

// eachRoot visits every expression tree that runs in production: function
// bodies and package-level variable initializers. params are the enclosing
// function's parameters, nil for initializers.
func (p *program) eachRoot(visit func(pkg *packages.Package, root ast.Node, params []*types.Var)) {
	for _, pkg := range p.pkgs {
		for _, file := range pkg.Syntax {
			if p.skipFile(file) {
				continue
			}
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if d.Body != nil {
						visit(pkg, d.Body, paramObjects(pkg.TypesInfo, d))
					}
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						if vs, ok := spec.(*ast.ValueSpec); ok {
							for _, value := range vs.Values {
								visit(pkg, value, nil)
							}
						}
					}
				}
			}
		}
	}
}

// packageByPath returns the loaded package with the given import path.
func (p *program) packageByPath(path string) *packages.Package {
	for _, pkg := range p.pkgs {
		if pkg.PkgPath == path {
			return pkg
		}
	}
	return nil
}

// skipFile reports whether a file is a test or generated file. Rules only
// judge hand-written production code.
func (p *program) skipFile(file *ast.File) bool {
	name := p.fset.Position(file.Pos()).Filename
	if strings.HasSuffix(name, "_test.go") || ast.IsGenerated(file) {
		return true
	}
	return strings.Contains(strings.ReplaceAll(name, "\\", "/"), "/generated/")
}

// calleeFunc resolves the function object a call targets, unwrapping
// parentheses and generic instantiation. Nil for dynamic calls and
// conversions.
func calleeFunc(info *types.Info, call *ast.CallExpr) *types.Func {
	var ident *ast.Ident
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		ident = fun
	case *ast.SelectorExpr:
		ident = fun.Sel
	case *ast.IndexExpr:
		return calleeFunc(info, &ast.CallExpr{Fun: fun.X})
	case *ast.IndexListExpr:
		return calleeFunc(info, &ast.CallExpr{Fun: fun.X})
	default:
		return nil
	}
	fn, _ := info.Uses[ident].(*types.Func)
	return fn
}

func pkgPathOf(obj types.Object) string {
	if obj == nil || obj.Pkg() == nil {
		return ""
	}
	return obj.Pkg().Path()
}

func isHumaPkgPath(path string) bool {
	return path == humaPath || strings.HasPrefix(path, humaPath+"/")
}

// isHumaNamed reports whether t is the named type huma.<name> (pointers are
// looked through).
func isHumaNamed(t types.Type, name string) bool {
	if t == nil {
		return false
	}
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj != nil && obj.Name() == name && pkgPathOf(obj) == humaPath
}

// constString returns the constant string value of expr, if it has one.
func constString(info *types.Info, expr ast.Expr) (string, bool) {
	tv, ok := info.Types[expr]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

// rootIdent returns the leftmost identifier of a selector chain such as
// s.cfg.Formats, or the identifier itself.
func rootIdent(expr ast.Expr) *ast.Ident {
	for {
		switch e := ast.Unparen(expr).(type) {
		case *ast.Ident:
			return e
		case *ast.SelectorExpr:
			expr = e.X
		case *ast.IndexExpr:
			expr = e.X
		case *ast.StarExpr:
			expr = e.X
		case *ast.UnaryExpr:
			expr = e.X
		default:
			return nil
		}
	}
}

// selectorOf returns pkg-qualified selector parts for expressions like
// huma.DefaultFormats: the package path and the selected object.
func selectorOf(info *types.Info, expr ast.Expr) (pkgPath string, obj types.Object) {
	sel, ok := ast.Unparen(expr).(*ast.SelectorExpr)
	if !ok {
		return "", nil
	}
	obj = info.Uses[sel.Sel]
	if obj == nil {
		return "", nil
	}
	return pkgPathOf(obj), obj
}

// paramObjects returns the parameter variables of fn in declaration order.
func paramObjects(info *types.Info, fn *ast.FuncDecl) []*types.Var {
	var params []*types.Var
	if fn.Type.Params == nil {
		return nil
	}
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			params = append(params, nil)
			continue
		}
		for _, name := range field.Names {
			v, _ := info.Defs[name].(*types.Var)
			params = append(params, v)
		}
	}
	return params
}

// stringParamIndex maps each string-typed parameter of fn to its index.
func stringParamIndex(info *types.Info, fn *ast.FuncDecl) map[*types.Var]int {
	params := map[*types.Var]int{}
	for i, param := range paramObjects(info, fn) {
		if param == nil {
			continue
		}
		if basic, ok := param.Type().Underlying().(*types.Basic); ok && basic.Kind() == types.String {
			params[param] = i
		}
	}
	return params
}

// mentionedParams returns the indexes of the given parameters that expr
// references.
func mentionedParams(info *types.Info, expr ast.Expr, params map[*types.Var]int) []int {
	var found []int
	ast.Inspect(expr, func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok {
			if v, ok := info.Uses[ident].(*types.Var); ok {
				if idx, ok := params[v]; ok && !slices.Contains(found, idx) {
					found = append(found, idx)
				}
			}
		}
		return true
	})
	return found
}

// flowSet records, per module function, which parameter indexes flow into
// a position of interest (a request URL or a route path).
type flowSet map[*types.Func][]int

func (f flowSet) add(fn *types.Func, indexes []int) bool {
	changed := false
	for _, idx := range indexes {
		if !slices.Contains(f[fn], idx) {
			f[fn] = append(f[fn], idx)
			changed = true
		}
	}
	return changed
}

// eachStringConst visits the outermost constant string sub-expressions of
// expr, so a folded concatenation is seen whole and a format string inside
// fmt.Sprintf is still reached.
func eachStringConst(info *types.Info, expr ast.Expr, visit func(ast.Expr)) {
	ast.Inspect(expr, func(n ast.Node) bool {
		e, ok := n.(ast.Expr)
		if !ok {
			return true
		}
		if _, ok := constString(info, e); ok {
			visit(e)
			return false
		}
		return true
	})
}
