package humacheck

import (
	"go/ast"
	"go/types"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

// routeSet is the inventory of HTTP routes the module registers with Huma.
type routeSet struct {
	adapterPrefixes []string // mount prefixes of adapters; "" when an adapter is mounted bare
	groupPrefixes   []string // prefixes given to huma.NewGroup
	paths           []string // registered operation paths
	concrete        []string // every reachable prefix + path
	// kinds records the receiver each path was registered on; a path seen
	// on more than one kind of receiver is receiverUnknown.
	kinds map[string]receiverKind
}

// receiverKind classifies the API value an operation was registered on.
type receiverKind int

const (
	receiverUnknown receiverKind = iota // not determinable; composed both ways
	receiverAPI                         // a plain API: adapter prefix + path
	receiverGroup                       // a *huma.Group: adapter prefix + group prefix + path
)

var paramPattern = regexp.MustCompile(`\{[^/}]*\}`)

// collectRoutes gathers operation paths from huma.Operation literals, the
// huma.Get/Post/... helpers, and module helpers whose string parameter
// becomes an operation path, plus the mount prefixes of adapters and
// groups. A path registered on a *huma.Group composes as adapter prefix +
// group prefix + path; a path registered on a plain API composes as adapter
// prefix + path; a path whose receiver cannot be determined (a helper's API
// parameter, a standalone Operation literal) composes both ways. The
// checker does not track which group or adapter a route belongs to, so with
// several groups or adapters their prefixes can still combine with each
// other's paths; that only matters when a literal spells such a combination.
func collectRoutes(p *program) *routeSet {
	rs := &routeSet{kinds: map[string]receiverKind{}}
	registrars := p.registrars()
	addPath := func(path string, kind receiverKind) {
		if path == "" || !strings.HasPrefix(path, "/") {
			return
		}
		if seen, ok := rs.kinds[path]; ok {
			if seen != kind {
				rs.kinds[path] = receiverUnknown
			}
			return
		}
		rs.kinds[path] = kind
		rs.paths = append(rs.paths, path)
	}
	addUnique := func(list *[]string, value string) {
		if !slices.Contains(*list, value) {
			*list = append(*list, value)
		}
	}
	handled := map[ast.Node]bool{}

	p.eachRoot(func(pkg *packages.Package, root ast.Node, params []*types.Var) {
		info := pkg.TypesInfo
		// pathsIn adds every route spelled inside expr: Operation literal
		// paths and constant arguments at registrar path positions.
		pathsIn := func(expr ast.Expr, kind receiverKind) {
			ast.Inspect(expr, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CompositeLit:
					if isHumaNamed(info.TypeOf(node), "Operation") {
						handled[node] = true
						if value := operationPathValue(node); value != nil {
							if path, ok := constString(info, value); ok {
								addPath(path, kind)
							}
						}
					}
				case *ast.CallExpr:
					callee := calleeFunc(info, node)
					if callee == nil {
						return true
					}
					for _, idx := range pathArgIndexes(callee, registrars) {
						handled[node] = true
						if idx < len(node.Args) {
							if route, ok := constString(info, node.Args[idx]); ok {
								addPath(route, kind)
							}
						}
					}
				}
				return true
			})
		}
		ast.Inspect(root, func(n ast.Node) bool {
			if handled[n] {
				return true
			}
			switch node := n.(type) {
			case *ast.CompositeLit:
				if isHumaNamed(info.TypeOf(node), "Operation") {
					pathsIn(node, receiverUnknown)
				}
			case *ast.CallExpr:
				callee := calleeFunc(info, node)
				if callee == nil {
					return true
				}
				path := pkgPathOf(callee)
				switch {
				case path == humaPath && callee.Name() == "NewGroup":
					for _, arg := range node.Args[min(1, len(node.Args)):] {
						if prefix, ok := constString(info, arg); ok && strings.HasPrefix(prefix, "/") {
							addUnique(&rs.groupPrefixes, strings.TrimSuffix(prefix, "/"))
						}
					}
				case isHumaPkgPath(path) && path != humaPath && constructsAPI(callee):
					// Adapter constructors: a constant string argument is the
					// mount prefix; without one the adapter is mounted bare.
					prefix := ""
					for _, arg := range node.Args {
						if value, ok := constString(info, arg); ok {
							prefix = strings.TrimSuffix(value, "/")
						}
					}
					addUnique(&rs.adapterPrefixes, prefix)
				case (path == humaPath && (callee.Name() == "Register" || isHumaMethodHelper(callee.Name()))) || len(pathArgIndexes(callee, registrars)) > 0:
					kind := receiverKindOf(info, node, params)
					handled[node] = true
					for _, arg := range node.Args {
						pathsIn(arg, kind)
					}
					for _, idx := range pathArgIndexes(callee, registrars) {
						if idx < len(node.Args) {
							if route, ok := constString(info, node.Args[idx]); ok {
								addPath(route, kind)
							}
						}
					}
				}
			}
			return true
		})
	})

	if len(rs.adapterPrefixes) == 0 {
		rs.adapterPrefixes = []string{""}
	}
	for _, adapter := range rs.adapterPrefixes {
		for _, path := range rs.paths {
			kind := rs.kinds[path]
			if kind != receiverGroup {
				addUnique(&rs.concrete, adapter+path)
			}
			if kind != receiverAPI {
				for _, group := range rs.groupPrefixes {
					addUnique(&rs.concrete, adapter+group+path)
				}
			}
		}
	}
	return rs
}

// receiverKindOf classifies the API argument of a registration call: the
// first argument typed as a Huma API or Group. A *huma.Group is a group
// receiver; a value that came in as a parameter of the enclosing function
// could be either; anything else is a plain API.
func receiverKindOf(info *types.Info, call *ast.CallExpr, params []*types.Var) receiverKind {
	for _, arg := range call.Args {
		t := info.TypeOf(arg)
		switch {
		case isHumaNamed(t, "Group"):
			return receiverGroup
		case isHumaNamed(t, "API"):
			if ident := rootIdent(arg); ident != nil {
				if v, ok := info.Uses[ident].(*types.Var); ok && slices.Contains(params, v) {
					return receiverUnknown
				}
			}
			return receiverAPI
		}
	}
	return receiverUnknown
}

// constructsAPI reports whether fn returns a Huma API or Adapter, which is
// what the adapter packages' New, NewWithPrefix, and NewAdapter do.
func constructsAPI(fn *types.Func) bool {
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return false
	}
	for res := range sig.Results().Variables() {
		if isHumaNamed(res.Type(), "API") || isHumaNamed(res.Type(), "Adapter") {
			return true
		}
	}
	return false
}

// operationPathValue returns the Path field value of an Operation literal.
func operationPathValue(lit *ast.CompositeLit) ast.Expr {
	for _, elt := range lit.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Path" {
				return kv.Value
			}
		}
	}
	return nil
}

func isHumaMethodHelper(name string) bool {
	switch name {
	case "Get", "Post", "Put", "Patch", "Delete", "Head", "Options":
		return true
	}
	return false
}

// pathArgIndexes returns the argument positions of a call to fn that become
// an operation path: the path argument of huma.Get and friends, or the
// string parameters a module registrar forwards into one.
func pathArgIndexes(fn *types.Func, registrars flowSet) []int {
	if pkgPathOf(fn) == humaPath && isHumaMethodHelper(fn.Name()) {
		return []int{1}
	}
	return registrars[fn]
}

// registrars computes, for every module function, the string parameters
// that become an operation path: in an Operation literal's Path field, in a
// Huma method helper, or forwarded into another registrar. Constant
// arguments at those positions are routes.
func (p *program) registrars() flowSet {
	set := flowSet{}
	for changed := true; changed; {
		changed = false
		for fn, fd := range p.funcs {
			info := fd.pkg.TypesInfo
			params := stringParamIndex(info, fd.decl)
			if len(params) == 0 {
				continue
			}
			var found []int
			ast.Inspect(fd.decl.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if ok && isHumaNamed(info.TypeOf(lit), "Operation") {
					if value := operationPathValue(lit); value != nil {
						found = append(found, mentionedParams(info, value, params)...)
					}
				}
				return true
			})
			found = append(found, forwardedParams(fd, func(callee *types.Func) []int {
				return pathArgIndexes(callee, set)
			})...)
			if set.add(fn, found) {
				changed = true
			}
		}
	}
	return set
}

// match reports the registered route that a hand-written URL literal hits,
// if any. The literal may carry a base URL, format verbs, template holes, a
// query string, concrete parameter values, or a trailing slash awaiting a
// concatenated identifier.
func (rs *routeSet) match(literal string) (string, bool) {
	if rs == nil || len(rs.concrete) == 0 {
		return "", false
	}
	path := extractPath(literal)
	if path == "" || path == "/" || strings.Contains(path, "//") {
		return "", false
	}
	open := strings.HasSuffix(path, "/")
	segments := strings.Split(strings.Trim(path, "/"), "/")
	// A path made only of placeholders ("/%s", "/{id}") would match every
	// single-segment route; require at least one literal segment.
	if !slices.ContainsFunc(segments, func(seg string) bool { return !wildSegment(seg) }) {
		return "", false
	}
	for _, route := range rs.concrete {
		if segmentsMatch(segments, open, strings.Split(strings.TrimPrefix(route, "/"), "/")) {
			return paramPattern.ReplaceAllString(route, "{param}"), true
		}
	}
	return "", false
}

var verbPattern = regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z]`)

// wildSegment reports whether a literal segment stands for a runtime value:
// a format verb, a template hole, or a {param}.
func wildSegment(seg string) bool {
	return verbPattern.MatchString(seg) || paramPattern.MatchString(seg)
}

// segmentsMatch compares a literal path to a route segment by segment. Route
// parameters and literal placeholders match any single segment; an open
// literal (trailing slash) must be a proper prefix of the route; a route
// ending in a {name...} wildcard absorbs the remaining literal segments.
func segmentsMatch(literal []string, open bool, route []string) bool {
	if len(route) == 0 {
		return false
	}
	last := route[len(route)-1]
	wildcardTail := strings.HasPrefix(last, "{") && strings.HasSuffix(last, "...}")
	switch {
	case open:
		if len(route) <= len(literal) {
			return false
		}
		route = route[:len(literal)]
	case wildcardTail:
		if len(literal) < len(route) {
			return false
		}
		literal = literal[:len(route)-1]
		route = route[:len(route)-1]
	case len(literal) != len(route):
		return false
	}
	for i := range route {
		if paramPattern.MatchString(route[i]) || wildSegment(literal[i]) {
			continue
		}
		if route[i] != literal[i] {
			return false
		}
	}
	return true
}

// extractPath pulls the URL path portion out of a string literal: the text
// from the first "/" after any scheme and host up to a query, fragment, or
// whitespace. Prose with spaces before the path is not a URL.
func extractPath(literal string) string {
	s := literal
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	i := strings.IndexByte(s, '/')
	if i < 0 || strings.ContainsAny(s[:i], " \t\n") {
		return ""
	}
	s = s[i:]
	if j := strings.IndexAny(s, "?# \t\n\"'`"); j >= 0 {
		s = s[:j]
	}
	return s
}
