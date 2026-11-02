// Package sqlownership extends the SQL analyzers with ownership transfers and
// cross-package summaries of scanners that check iteration errors.
package sqlownership

import (
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"slices"

	"github.com/golangci/rowserrcheck/passes/rowserr"
	sqlclose "github.com/ryanrolds/sqlclosecheck/pkg/analyzer"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/analysis/passes/ctrlflow"
	"golang.org/x/tools/go/ssa"
)

type scannerFact struct{ Parameters []int }

func (*scannerFact) AFact() {}

type (
	handled struct{ returned, scanned bool }
	result  map[token.Pos]handled
	// imported maps functions from any package, the current one included,
	// to the parameters they check as scanners.
	imported map[*types.Func][]int
)

// scanners exports scanner facts. Facts make it run on every dependency, so
// it does not require buildssa: it builds SSA itself, and only for packages
// that declare an exported function taking rows.
var scanners = &analysis.Analyzer{
	Name: "sqlscanners", Doc: "Summarizes functions that check SQL rows iteration errors",
	Requires:   []*analysis.Analyzer{ctrlflow.Analyzer},
	FactTypes:  []analysis.Fact{new(scannerFact)},
	ResultType: reflect.TypeFor[imported](), Run: exportScanners,
}

// ownership has no facts, so it and buildssa run only on analyzed packages.
var ownership = &analysis.Analyzer{
	Name: "sqlownership", Doc: "Tracks SQL resource transfers and scanner error checks",
	Requires:   []*analysis.Analyzer{buildssa.Analyzer, ctrlflow.Analyzer, scanners},
	ResultType: reflect.TypeFor[result](), Run: analyze,
}

// CloseAnalyzer retains sqlclosecheck's diagnostics except for proven transfers.
var CloseAnalyzer = extend(sqlclose.NewAnalyzer(), false)

// ErrAnalyzer retains rowserrcheck's diagnostics except for transfers and checked scanners.
var ErrAnalyzer = extend(rowserr.NewAnalyzer(), true)

func extend(base *analysis.Analyzer, iteration bool) *analysis.Analyzer {
	a := *base
	a.Requires = append(append([]*analysis.Analyzer{}, base.Requires...), ownership)
	a.Run = func(pass *analysis.Pass) (any, error) {
		transfers := pass.ResultOf[ownership].(result)
		copyPass := *pass
		copyPass.Report = func(d analysis.Diagnostic) {
			h := transfers[d.Pos]
			if h.returned || iteration && h.scanned {
				return
			}
			pass.Report(d)
		}
		return base.Run(&copyPass)
	}
	return &a
}

type summaries map[*ssa.Function]map[int]bool

func exportScanners(pass *analysis.Pass) (any, error) {
	if declaresScanner(pass) {
		prog := ssa.NewProgram(pass.Fset, ssa.BuilderMode(0))
		prog.SetNoReturn(pass.ResultOf[ctrlflow.Analyzer].(*ctrlflow.CFGs).NoReturn)
		for _, p := range pass.Pkg.Imports() {
			prog.CreatePackage(p, nil, nil, true)
		}
		pkg := prog.CreatePackage(pass.Pkg, pass.Files, pass.TypesInfo, false)
		pkg.Build()
		// The source functions in source order, as buildssa lists them.
		var funcs []*ssa.Function
		for _, file := range pass.Files {
			for _, decl := range file.Decls {
				if decl, ok := decl.(*ast.FuncDecl); ok {
					funcs = appendWithAnons(funcs, prog.FuncValue(pass.TypesInfo.Defs[decl.Name].(*types.Func)))
				}
			}
		}
		lookup := func(obj *types.Func) []int {
			var fact scannerFact
			if pass.ImportObjectFact(obj, &fact) {
				return fact.Parameters
			}
			return nil
		}
		scans := summarize(funcs, lookup)
		for _, fn := range funcs {
			obj, ok := fn.Object().(*types.Func)
			if !ok {
				continue
			}
			fact := new(scannerFact)
			for i := range fn.Params {
				if scans[fn][i] {
					fact.Parameters = append(fact.Parameters, i)
				}
			}
			if len(fact.Parameters) > 0 {
				pass.ExportObjectFact(obj, fact)
			}
		}
	}
	known := imported{}
	for _, f := range pass.AllObjectFacts() {
		if obj, ok := f.Object.(*types.Func); ok {
			known[obj] = f.Fact.(*scannerFact).Parameters
		}
	}
	return known, nil
}

func appendWithAnons(funcs []*ssa.Function, fn *ssa.Function) []*ssa.Function {
	funcs = append(funcs, fn)
	for _, anon := range fn.AnonFuncs {
		funcs = appendWithAnons(funcs, anon)
	}
	return funcs
}

// declaresScanner reports whether the package declares an exported function
// or method with a parameter that can receive rows. Only those can carry a
// fact another package uses; packages without one skip SSA construction.
func declaresScanner(pass *analysis.Pass) bool {
	for _, obj := range pass.TypesInfo.Defs {
		fn, ok := obj.(*types.Func)
		if !ok || !fn.Exported() {
			continue
		}
		sig := fn.Signature()
		if recv := sig.Recv(); recv != nil && mayHoldRows(recv.Type()) {
			return true
		}
		for param := range sig.Params().Variables() {
			if mayHoldRows(param.Type()) {
				return true
			}
		}
	}
	return false
}

// mayHoldRows reports whether a parameter of type t can receive rows that the
// scanner analysis follows: a resource or a type converted from one, or an
// interface or type parameter with Err() error. A defined row type can add
// any methods, so the rest of the interface does not matter. The exception
// is context.Context: no row type is also a context, and treating every
// context parameter as a possible scanner would build SSA for nearly every
// package.
func mayHoldRows(t types.Type) bool {
	t = types.Unalias(t)
	u := t.Underlying()
	// A type parameter's underlying type is its constraint interface.
	if iface, ok := u.(*types.Interface); ok {
		return !isContext(t) && hasErr(iface)
	}
	if resource(t) || resource(u) {
		return true
	}
	// A defined type over a resource shares its unexported fields, which
	// belong to the resource's package.
	if p, ok := u.(*types.Pointer); ok {
		u = p.Elem().Underlying()
	}
	if s, ok := u.(*types.Struct); ok {
		for field := range s.Fields() {
			if !field.Exported() && field.Pkg() != nil && resourcePackage(field.Pkg().Path()) {
				return true
			}
		}
	}
	return false
}

func hasErr(iface *types.Interface) bool {
	for m := range iface.Methods() {
		if m.Name() == "Err" && m.Signature().Params().Len() == 0 && returnsError(m.Signature()) {
			return true
		}
	}
	return false
}

func isContext(t types.Type) bool {
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == "context" && n.Obj().Name() == "Context"
}

// summarize finds the parameters each function checks as a scanner. It
// reaches a fixed point for scanners forwarding to other local scanners.
func summarize(funcs []*ssa.Function, lookup func(*types.Func) []int) summaries {
	scans := summaries{}
	for changed := true; changed; {
		changed = false
		for _, fn := range funcs {
			for i, param := range fn.Params {
				if scans[fn][i] || !mayHoldRows(param.Type()) {
					continue
				}
				if checked(scans, lookup, param, map[ssa.Value]bool{}) {
					if scans[fn] == nil {
						scans[fn] = map[int]bool{}
					}
					scans[fn][i] = true
					changed = true
				}
			}
		}
	}
	return scans
}

func analyze(pass *analysis.Pass) (any, error) {
	program := pass.ResultOf[buildssa.Analyzer].(*buildssa.SSA)
	known := pass.ResultOf[scanners].(imported)
	lookup := func(obj *types.Func) []int { return known[obj] }
	scans := summarize(program.SrcFuncs, lookup)
	out := result{}
	for _, fn := range program.SrcFuncs {
		for _, block := range fn.Blocks {
			for _, instr := range block.Instrs {
				call, ok := instr.(*ssa.Call)
				if !ok {
					continue
				}
				values := []ssa.Value{call}
				if _, tuple := call.Type().(*types.Tuple); tuple {
					values = nil
					for _, ref := range refs(call) {
						if v, ok := ref.(*ssa.Extract); ok {
							values = append(values, v)
						}
					}
				}
				for _, value := range values {
					if !resource(value.Type()) {
						continue
					}
					h, exists := out[call.Pos()]
					if !exists {
						h = handled{returned: true, scanned: true}
					}
					// SSA proves the value flows into a return; the path
					// search proves no other path drops it.
					transferred := returned(value, map[ssa.Value]bool{}) && everyPath(pass, call.Pos())
					h.returned = h.returned && transferred
					h.scanned = h.scanned && (transferred || checked(scans, lookup, value, map[ssa.Value]bool{}))
					out[call.Pos()] = h
				}
			}
		}
	}
	return out, nil
}

func refs(v ssa.Value) []ssa.Instruction {
	if v.Referrers() == nil {
		return nil
	}
	return *v.Referrers()
}

func resource(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	if !ok || n.Obj().Pkg() == nil || !resourcePackage(n.Obj().Pkg().Path()) {
		return false
	}
	switch n.Obj().Name() {
	case "Rows", "Stmt", "NamedStmt":
		return true
	}
	return false
}

func resourcePackage(path string) bool {
	switch path {
	case "database/sql", "github.com/jmoiron/sqlx", "github.com/jackc/pgx/v5", "github.com/jackc/pgx/v5/pgxpool":
		return true
	}
	return false
}

// returned follows the specific resource, including named-result stack slots
// emitted by SSA for functions with defers. Storing in an unrelated object or
// returning some other resource does not transfer this resource.
func returned(v ssa.Value, seen map[ssa.Value]bool) bool {
	if seen[v] {
		return false
	}
	seen[v] = true
	for _, ref := range refs(v) {
		switch r := ref.(type) {
		case *ssa.Return:
			if slices.Contains(r.Results, v) {
				return true
			}
		case *ssa.MakeInterface:
			if returned(r, seen) {
				return true
			}
		case *ssa.ChangeType:
			if returned(r, seen) {
				return true
			}
		case *ssa.Phi:
			if returned(r, seen) {
				return true
			}
		case *ssa.Store:
			if r.Val != v {
				continue
			}
			if field, ok := r.Addr.(*ssa.FieldAddr); ok {
				if returned(field.X, seen) {
					return true
				}
			} else if _, local := r.Addr.(*ssa.Alloc); local {
				for _, use := range refs(r.Addr) {
					if load, ok := use.(*ssa.UnOp); ok && load.Op == token.MUL && returned(load, seen) {
						return true
					}
				}
			}
		case *ssa.UnOp:
			if r.Op == token.MUL && returned(r, seen) {
				return true
			}
		}
	}
	return false
}

func checked(scans summaries, lookup func(*types.Func) []int, v ssa.Value, seen map[ssa.Value]bool) bool {
	if seen[v] {
		return false
	}
	seen[v] = true
	for _, ref := range refs(v) {
		switch r := ref.(type) {
		case *ssa.MakeInterface:
			if checked(scans, lookup, r, seen) {
				return true
			}
		case *ssa.ChangeType:
			if checked(scans, lookup, r, seen) {
				return true
			}
		case *ssa.Phi:
			if checked(scans, lookup, r, seen) {
				return true
			}
		case *ssa.Call:
			common := r.Common()
			if errReceiver(common) == v && len(refs(r)) > 0 {
				return true
			}
			callee := common.StaticCallee()
			if callee == nil || ignoresError(r) {
				continue
			}
			for i, arg := range common.Args {
				if arg != v {
					continue
				}
				if scans[callee][i] {
					return true
				}
				if obj, ok := callee.Object().(*types.Func); ok && slices.Contains(lookup(obj), i) {
					return true
				}
			}
		}
	}
	return false
}

// ignoresError catches discarded scanner errors without trying to prove how
// callers handle errors they use. Scanners with no error result handle it inside.
func ignoresError(call *ssa.Call) bool {
	results := call.Common().Signature().Results()
	for i := range results.Len() {
		if !types.Identical(results.At(i).Type(), types.Universe.Lookup("error").Type()) {
			continue
		}
		if results.Len() == 1 {
			return len(refs(call)) == 0
		}
		used := false
		for _, ref := range refs(call) {
			if extract, ok := ref.(*ssa.Extract); ok && extract.Index == i && len(refs(extract)) > 0 {
				used = true
			}
		}
		if !used {
			return true
		}
	}
	return false
}

func errReceiver(call *ssa.CallCommon) ssa.Value {
	if call.IsInvoke() {
		if call.Method.Name() == "Err" && call.Signature().Params().Len() == 0 && returnsError(call.Signature()) {
			return call.Value
		}
	} else if fn := call.StaticCallee(); fn != nil && fn.Name() == "Err" && fn.Signature.Recv() != nil && fn.Signature.Params().Len() == 0 && returnsError(fn.Signature) && len(call.Args) == 1 {
		return call.Args[0]
	}
	return nil
}

func returnsError(sig *types.Signature) bool {
	return sig.Results().Len() == 1 && types.Identical(sig.Results().At(0).Type(), types.Universe.Lookup("error").Type())
}
