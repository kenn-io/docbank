package humacheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

const (
	jsonV1ImportMessage  = "encoding/json (v1) is imported; use encoding/json/v2 and encoding/json/jsontext so Huma payloads get v2 semantics (nil slices encode as [], not null)"
	jsonV2MissingMessage = "this module builds a Huma API but never installs an encoding/json/v2 huma.Format; write a huma.Format whose Marshal and Unmarshal use encoding/json/v2 into config.Formats[\"application/json\"] or huma.DefaultFormats[\"application/json\"] (assigning huma.DefaultJSONFormat alone changes nothing: huma.DefaultFormats already copied the v1 value)"
)

// verdict is a three-valued answer for "does this use JSON v2".
type verdict int

const (
	unknown verdict = iota
	yes
	no
)

const maxDepth = 6

// constructionSite is a call that hands a huma.Config to Huma or builds a
// Huma adapter; its presence means the module serves a Huma API.
type constructionSite struct {
	pkg  *packages.Package
	call *ast.CallExpr
}

// constructionSites finds every production call into a Huma package that
// takes a huma.Config or returns a huma.API or huma.Adapter, in function
// bodies and package-level initializers alike.
func (p *program) constructionSites() []constructionSite {
	var sites []constructionSite
	p.eachRoot(func(pkg *packages.Package, root ast.Node, _ []*types.Var) {
		ast.Inspect(root, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee := calleeFunc(pkg.TypesInfo, call)
			if callee == nil || !isHumaPkgPath(pkgPathOf(callee)) {
				return true
			}
			if takesConfig(callee) || constructsAPI(callee) {
				sites = append(sites, constructionSite{pkg: pkg, call: call})
			}
			return true
		})
	})
	return sites
}

func takesConfig(fn *types.Func) bool {
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return false
	}
	for param := range sig.Params().Variables() {
		if isHumaNamed(param.Type(), "Config") {
			return true
		}
	}
	return false
}

// checkJSONV2 enforces JSON v2 with two module-wide checks and no value
// tracing: no production file in the module imports encoding/json (v1),
// and a module that builds a Huma API installs at least one huma.Format
// backed by encoding/json/v2 somewhere. Huma's own defaults encode with v1
// inside the huma package, so a module with no v1 import still needs the
// install.
//
// The import ban reads every tracked Go file under the module directory
// rather than the loaded packages, so files excluded by build tags or
// platform suffixes are covered too.
func checkJSONV2(p *program, repo fs.FS, tracked []string, hasAPI bool, goModPath string) ([]Diagnostic, error) {
	var diags []Diagnostic
	moduleDir := path.Dir(goModPath)
	nested := nestedModuleDirs(tracked, moduleDir)
	for _, name := range tracked {
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || !underDir(name, moduleDir) || excludedDir(name) || strings.Contains("/"+name, "/generated/") {
			continue
		}
		if slices.ContainsFunc(nested, func(dir string) bool { return underDir(name, dir) }) {
			continue // owned by a nested module
		}
		content, err := readTracked(repo, name)
		if err != nil {
			return nil, err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, content, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			// A file the rule cannot read must not pass silently, and a file
			// outside the build context is never reported by the loader.
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		if ast.IsGenerated(file) {
			continue
		}
		for _, imp := range file.Imports {
			if importPath(imp) == jsonV1Path {
				pos := fset.Position(imp.Pos())
				diags = append(diags, Diagnostic{Path: name, Line: pos.Line, Column: pos.Column, Rule: RuleJSONV2, Message: jsonV1ImportMessage})
			}
		}
	}
	if hasAPI && !newFormatFinder(p).installsV2() {
		diags = append(diags, Diagnostic{Path: goModPath, Line: 1, Column: 1, Rule: RuleJSONV2, Message: jsonV2MissingMessage})
	}
	return diags, nil
}

func importPath(imp *ast.ImportSpec) string {
	path, err := strconv.Unquote(imp.Path.Value)
	if err != nil {
		return ""
	}
	return path
}

// formatFinder looks for one huma.Format install backed by JSON v2 anywhere
// in the module. It is an existence check: which API the format reaches is
// not tracked.
type formatFinder struct {
	p           *program
	formatFuncs map[*types.Func]verdict
	// defaultOverridden is set while walking one function body, once
	// huma.DefaultJSONFormat has been assigned a v2 format earlier in that
	// body. The assignment alone installs nothing: huma.DefaultFormats
	// copied the old value at package init and huma.DefaultConfig reads the
	// map, so the variable only counts when the same body then writes it
	// into a Formats map.
	defaultOverridden bool
}

func newFormatFinder(p *program) *formatFinder {
	return &formatFinder{p: p, formatFuncs: make(map[*types.Func]verdict)}
}

// installsV2 reports whether some application/json map install carries a
// v2 format. Installs are writes to config.Formats["application/json"] or
// huma.DefaultFormats["application/json"], a Formats map assigned to a
// huma.Config, and the Formats entry of a huma.Config literal. A format map
// that never reaches a Config or Huma's defaults does not count.
func (f *formatFinder) installsV2() bool {
	found := false
	f.p.eachRoot(func(pkg *packages.Package, root ast.Node, _ []*types.Var) {
		if found {
			return
		}
		f.defaultOverridden = false
		ast.Inspect(root, func(n ast.Node) bool {
			if found {
				return false
			}
			switch node := n.(type) {
			case *ast.FuncLit:
				// A closure is its own body for ordering purposes; it is
				// visited as part of this root but must not inherit or leak
				// the override flag.
				saved := f.defaultOverridden
				f.defaultOverridden = false
				ast.Inspect(node.Body, func(n ast.Node) bool { return f.visitInstall(pkg, n, &found) })
				f.defaultOverridden = saved
				return false
			default:
				return f.visitInstall(pkg, n, &found)
			}
		})
	})
	return found
}

// visitInstall handles one node of a body walk in source order: it records
// a v2 override of huma.DefaultJSONFormat and sets found when a map install
// carries a v2 format.
func (f *formatFinder) visitInstall(pkg *packages.Package, n ast.Node, found *bool) bool {
	info := pkg.TypesInfo
	switch node := n.(type) {
	case *ast.AssignStmt:
		if len(node.Lhs) != len(node.Rhs) {
			return true
		}
		// Go evaluates every right-hand side before any write, so the
		// override state changes only after the whole statement.
		overridden := f.defaultOverridden
		for i := range node.Lhs {
			lhs, rhs := node.Lhs[i], node.Rhs[i]
			if isDefault, ok := installTarget(info, lhs); ok {
				if isDefault {
					// The latest assignment in walk order decides; a later
					// v1 or unknown value clears an earlier override.
					overridden = f.formatExpr(pkg, rhs, 0) == yes
				} else if f.formatExpr(pkg, rhs, 0) == yes {
					*found = true
				}
				continue
			}
			if isFormatsField(info, lhs) && f.mapValue(pkg, rhs, 0) == yes {
				*found = true
			}
		}
		f.defaultOverridden = overridden
	case *ast.CompositeLit:
		if !isHumaNamed(info.TypeOf(node), "Config") {
			return true
		}
		for _, elt := range node.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Formats" && f.mapValue(pkg, kv.Value, 0) == yes {
				*found = true
			}
		}
	}
	return true
}

// isFormatsField reports whether lhs is the Formats field of a huma.Config.
func isFormatsField(info *types.Info, lhs ast.Expr) bool {
	sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Formats" && isHumaNamed(info.TypeOf(sel.X), "Config")
}

// mapValue judges a map[string]huma.Format expression by its
// application/json entry: a literal directly, or a variable through the
// literals assigned to it anywhere in its package.
func (f *formatFinder) mapValue(pkg *packages.Package, expr ast.Expr, depth int) verdict {
	if depth > maxDepth {
		return unknown
	}
	info := pkg.TypesInfo
	switch e := ast.Unparen(expr).(type) {
	case *ast.CompositeLit:
		for _, elt := range e.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := constString(info, kv.Key); ok && key == "application/json" {
				return f.formatExpr(pkg, kv.Value, depth+1)
			}
		}
		return unknown
	case *ast.Ident:
		obj, ok := info.Uses[e].(*types.Var)
		if !ok {
			return unknown
		}
		result := unknown
		eachAssignment(pkg, obj, func(rhs ast.Expr) {
			if lit, ok := ast.Unparen(rhs).(*ast.CompositeLit); ok && f.mapValue(pkg, lit, depth+1) == yes {
				result = yes
			}
		})
		return result
	}
	return unknown
}

// eachAssignment visits every value assigned to obj in its package,
// including its declaration initializer.
func eachAssignment(pkg *packages.Package, obj *types.Var, visit func(rhs ast.Expr)) {
	info := pkg.TypesInfo
	for _, file := range pkg.Syntax {
		ast.Inspect(file, func(n ast.Node) bool {
			var lhs, rhs []ast.Expr
			switch node := n.(type) {
			case *ast.AssignStmt:
				lhs, rhs = node.Lhs, node.Rhs
			case *ast.ValueSpec:
				for _, name := range node.Names {
					lhs = append(lhs, name)
				}
				rhs = node.Values
			default:
				return true
			}
			if len(lhs) != len(rhs) {
				return true
			}
			for i, l := range lhs {
				if ident, ok := ast.Unparen(l).(*ast.Ident); ok && (info.Defs[ident] == obj || info.Uses[ident] == obj) {
					visit(rhs[i])
				}
			}
			return true
		})
	}
}

// installTarget reports whether lhs is an application/json format slot
// that reaches Huma: huma.DefaultJSONFormat (isDefault), or the
// "application/json" index of a huma.Config's Formats field or of
// huma.DefaultFormats. An index write into any other format map is not an
// install.
func installTarget(info *types.Info, lhs ast.Expr) (isDefault bool, ok bool) {
	switch l := ast.Unparen(lhs).(type) {
	case *ast.SelectorExpr:
		if path, obj := selectorOf(info, l); obj != nil && path == humaPath && obj.Name() == "DefaultJSONFormat" {
			return true, true
		}
	case *ast.IndexExpr:
		if !isFormatsField(info, l.X) && !isHumaDefaultFormats(info, l.X) {
			return false, false
		}
		if key, ok := constString(info, l.Index); ok && key == "application/json" {
			return false, true
		}
	}
	return false, false
}

// isHumaDefaultFormats reports whether expr is huma.DefaultFormats.
func isHumaDefaultFormats(info *types.Info, expr ast.Expr) bool {
	path, obj := selectorOf(info, expr)
	return obj != nil && path == humaPath && obj.Name() == "DefaultFormats"
}

// formatExpr judges an expression of type huma.Format: yes only when both
// codecs are proven v2, no only when either is proven v1, else unknown.
func (f *formatFinder) formatExpr(pkg *packages.Package, expr ast.Expr, depth int) verdict {
	if depth > maxDepth {
		return unknown
	}
	info := pkg.TypesInfo
	switch e := ast.Unparen(expr).(type) {
	case *ast.CompositeLit:
		if !isHumaNamed(info.TypeOf(e), "Format") {
			return unknown
		}
		marshal, unmarshal := unknown, unknown
		for _, elt := range e.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			switch key.Name {
			case "Marshal":
				marshal = f.funcUsesV2(pkg, kv.Value, 0)
			case "Unmarshal":
				unmarshal = f.funcUsesV2(pkg, kv.Value, 0)
			}
		}
		switch {
		case marshal == no || unmarshal == no:
			return no
		case marshal == yes && unmarshal == yes:
			return yes
		}
		return unknown
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			return f.formatExpr(pkg, e.X, depth+1)
		}
	case *ast.SelectorExpr:
		pkgPath, obj := selectorOf(info, e)
		if obj == nil {
			return unknown
		}
		if pkgPath == humaPath && obj.Name() == "DefaultJSONFormat" {
			if f.defaultOverridden {
				return yes
			}
			return no
		}
		// A format defined in another module package, such as
		// codecs.JSONFormat, is judged in its defining package.
		if v, ok := obj.(*types.Var); ok {
			if home := f.p.packageByPath(pkgPath); home != nil {
				return f.varFormat(home, v, depth+1)
			}
		}
		return unknown
	case *ast.Ident:
		obj, ok := info.Uses[e].(*types.Var)
		if !ok {
			return unknown
		}
		if home := f.p.packageByPath(pkgPathOf(obj)); home != nil {
			pkg = home
		}
		return f.varFormat(pkg, obj, depth+1)
	}
	return unknown
}

// varFormat judges a variable by the values assigned to it anywhere in its
// package: yes when some assignment is a v2 format and none is v1.
func (f *formatFinder) varFormat(pkg *packages.Package, obj *types.Var, depth int) verdict {
	result := unknown
	eachAssignment(pkg, obj, func(rhs ast.Expr) {
		switch f.formatExpr(pkg, rhs, depth) {
		case no:
			result = no
		case yes:
			if result == unknown {
				result = yes
			}
		case unknown:
			// No evidence either way; keep the current verdict.
		}
	})
	return result
}

// funcUsesV2 judges a Marshal or Unmarshal value: a function literal or a
// reference to a function. It is yes only when the body reaches
// encoding/json/v2 and never calls an encoding/json (v1) codec function.
func (f *formatFinder) funcUsesV2(pkg *packages.Package, expr ast.Expr, depth int) verdict {
	info := pkg.TypesInfo
	switch e := ast.Unparen(expr).(type) {
	case *ast.FuncLit:
		return f.bodyUsesV2(pkg, e.Body, depth)
	case *ast.Ident, *ast.SelectorExpr:
		var obj types.Object
		if id, ok := e.(*ast.Ident); ok {
			obj = info.Uses[id]
		} else {
			_, obj = selectorOf(info, e)
		}
		fn, ok := obj.(*types.Func)
		if !ok {
			return unknown
		}
		return f.funcDeclUsesV2(fn, depth)
	}
	return unknown
}

func (f *formatFinder) funcDeclUsesV2(fn *types.Func, depth int) verdict {
	switch path := pkgPathOf(fn); {
	case path == jsonV2Path || path == jsonTextPath:
		return yes
	case path == jsonV1Path && isV1Codec(fn.Name()):
		return no
	}
	if v, ok := f.formatFuncs[fn]; ok {
		return v
	}
	fd, ok := f.p.funcs[fn]
	if !ok {
		return unknown
	}
	f.formatFuncs[fn] = unknown // cycle guard
	v := f.bodyUsesV2(fd.pkg, fd.decl.Body, depth+1)
	f.formatFuncs[fn] = v
	return v
}

// bodyUsesV2 scans calls in body. Local callees are followed so thin
// wrappers such as marshalAPIJSON count.
func (f *formatFinder) bodyUsesV2(pkg *packages.Package, body *ast.BlockStmt, depth int) verdict {
	if depth > maxDepth {
		return unknown
	}
	sawV2, sawV1 := false, false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn := calleeFunc(pkg.TypesInfo, call)
		if fn == nil {
			return true
		}
		switch path := pkgPathOf(fn); {
		case path == jsonV2Path || path == jsonTextPath:
			sawV2 = true
		case path == jsonV1Path && isV1Codec(fn.Name()):
			sawV1 = true
		default:
			if _, local := f.p.funcs[fn]; local {
				switch f.funcDeclUsesV2(fn, depth+1) {
				case yes:
					sawV2 = true
				case no:
					sawV1 = true
				case unknown:
					// The callee gives no evidence either way.
				}
			}
		}
		return true
	})
	switch {
	case sawV1:
		return no
	case sawV2:
		return yes
	}
	return unknown
}

func isV1Codec(name string) bool {
	switch name {
	case "Marshal", "MarshalIndent", "Unmarshal", "NewEncoder", "NewDecoder":
		return true
	}
	return false
}
