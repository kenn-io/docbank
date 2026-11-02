package humacheck

import (
	"go/ast"
	"go/token"
	"go/types"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

// checkClients reports string literals that name one of the module's own
// routes and are handed to code that builds HTTP requests from them.
func checkClients(p *program, routes *routeSet) []Diagnostic {
	if routes == nil || len(routes.concrete) == 0 {
		return nil
	}
	requesters := p.requesters()
	var diags []Diagnostic
	seen := map[token.Pos]bool{}
	report := func(pkg *packages.Package, expr ast.Expr) {
		literal, ok := constString(pkg.TypesInfo, expr)
		if !ok {
			return
		}
		route, ok := routes.match(literal)
		if !ok || seen[expr.Pos()] {
			return
		}
		seen[expr.Pos()] = true
		path, line, col := positionOf(p.fset, expr.Pos())
		diags = append(diags, Diagnostic{
			Path: path, Line: line, Column: col, Rule: RuleClient,
			Message: formatClientMessage(route),
		})
	}

	p.eachRoot(func(pkg *packages.Package, root ast.Node, _ []*types.Var) {
		info := pkg.TypesInfo
		ast.Inspect(root, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee := calleeFunc(info, call)
			if callee == nil {
				return true
			}
			for _, idx := range urlArgIndexes(callee, requesters) {
				if idx < len(call.Args) {
					eachStringConst(info, call.Args[idx], func(e ast.Expr) { report(pkg, e) })
				}
			}
			return true
		})
	})
	return diags
}

func formatClientMessage(route string) string {
	return "hand-rolled HTTP request to this module's own Huma route " + route + "; call it through the generated API client"
}

// urlArgIndexes returns the argument positions of a call to fn that end up
// as a request URL: the URL parameter of net/http builders, or the string
// parameters a module requester forwards into one.
func urlArgIndexes(fn *types.Func, requesters flowSet) []int {
	if idx, ok := httpURLArg(fn); ok {
		return []int{idx}
	}
	return requesters[fn]
}

// httpURLArg identifies net/http request builders and the index of their
// URL argument.
func httpURLArg(fn *types.Func) (int, bool) {
	if pkgPathOf(fn) != "net/http" {
		return 0, false
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return 0, false
	}
	if sig.Recv() != nil {
		if !isNamed(sig.Recv().Type(), "net/http", "Client") {
			return 0, false
		}
		switch fn.Name() {
		case "Get", "Post", "PostForm", "Head":
			return 0, true
		}
		return 0, false
	}
	switch fn.Name() {
	case "NewRequest":
		return 1, true
	case "NewRequestWithContext":
		return 2, true
	case "Get", "Post", "PostForm", "Head":
		return 0, true
	}
	return 0, false
}

func isNamed(t types.Type, pkgPath, name string) bool {
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj() == nil {
		return false
	}
	return named.Obj().Name() == name && pkgPathOf(named.Obj()) == pkgPath
}

// requesters computes, for every module function, the string parameters
// that reach a request URL: directly in a net/http builder, or by being
// forwarded into a URL position of another requester. Only those argument
// positions are inspected at call sites, so a body or log string that
// happens to look like a route is not reported. A //huma-check:external
// declaration marks a requester for a separate API, excluding its parameters.
func (p *program) requesters() flowSet {
	set := flowSet{}
	for changed := true; changed; {
		changed = false
		for fn, fd := range p.funcs {
			if fd.decl.Doc != nil && slices.ContainsFunc(fd.decl.Doc.List, func(comment *ast.Comment) bool {
				return strings.TrimSpace(strings.TrimPrefix(comment.Text, "//")) == "huma-check:external"
			}) {
				continue
			}
			if set.add(fn, forwardedParams(fd, func(callee *types.Func) []int {
				return urlArgIndexes(callee, set)
			})) {
				changed = true
			}
		}
	}
	return set
}

// forwardedParams returns the indexes of fd's string parameters that appear
// inside an argument of interest of some call in its body, where
// argsOfInterest names the interesting argument positions of a callee.
func forwardedParams(fd *funcDecl, argsOfInterest func(*types.Func) []int) []int {
	info := fd.pkg.TypesInfo
	params := stringParamIndex(info, fd.decl)
	if len(params) == 0 {
		return nil
	}
	var found []int
	ast.Inspect(fd.decl.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee := calleeFunc(info, call)
		if callee == nil {
			return true
		}
		for _, idx := range argsOfInterest(callee) {
			if idx < len(call.Args) {
				found = append(found, mentionedParams(info, call.Args[idx], params)...)
			}
		}
		return true
	})
	return found
}
