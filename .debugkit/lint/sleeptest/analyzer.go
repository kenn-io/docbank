// Package sleeptest reports time.Sleep calls in test files that run outside a
// testing/synctest bubble. Real sleeps make tests slow and timing-dependent;
// inside synctest.Test the sleep advances a fake clock instead.
//
// A bubble is the body of a function passed to synctest.Test (or the older
// synctest.Run): a literal written inline, or a declared function or
// function-valued variable passed by name. Sleeps anywhere inside that body,
// including nested closures and goroutines, are fine. A helper that sleeps
// and is merely called from inside a bubble cannot be recognized statically
// and is reported; give such helpers a channel to wait on or a sleep
// function to call instead.
//
// Test files are *_test.go files. With the helper-packages flag (on by
// default) files in packages named testutil or ending in "test", such as
// pkgtest, are checked too. The eventually flag (off by default) also
// reports testify's Eventually, EventuallyWithT, and Never, and their f
// variants, outside bubbles, for repositories that decide polling assertions
// should be replaced by signals.
//
// DeadlineAnalyzer (deadlinetest) reports sub-second wall-clock budgets in
// _test.go files outside a bubble: context.WithTimeout, WithTimeoutCause,
// WithDeadline, and WithDeadlineCause, time.After, NewTimer, and AfterFunc,
// and the waitFor argument of testify's Eventually and EventuallyWithT and
// their f variants. It also checks duration fields in literals and assignments,
// duration arguments to statically resolved same-module functions and methods,
// and standalone time.Now().Add deadlines. Duration fields and helper
// parameters must have timeout, deadline, budget, or wait as a camelCase
// word in their names; intervals, ticks, and delays are cadences, not
// budgets. Standalone Add checks apply regardless of the
// destination name. Duration elements in maps and slices, closures, and
// interface calls are out of scope. Same-module helper calls from _test.go files are checked;
// helper package source files themselves are not scanned. A short budget
// fails on a loaded CI runner even when the code is correct. Only constant
// budgets are reported. Zero and negative budgets are reported only for
// polling assertions, and Never and Neverf budgets only when zero or
// negative: a short Never checks fewer times under
// load but cannot fail, while one with no budget passes vacuously.
package sleeptest

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/types/typeutil"
)

// Analyzer reports time.Sleep calls in _test.go files outside synctest bubbles.
var Analyzer = &analysis.Analyzer{
	Name:     "sleeptest",
	Doc:      "reports time.Sleep in test files outside a testing/synctest bubble",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

const diagnosticMessage = "time.Sleep in a test outside a synctest bubble; wait on a channel or signal, or run the test under testing/synctest"

// HelperPackages extends the check from _test.go files to every file in a
// package named testutil or with a name ending in "test".
var HelperPackages = true

// Eventually also reports testify's Eventually, EventuallyWithT, and Never
// outside a bubble. They poll on a wall clock just like a sleep does.
var Eventually bool

func init() {
	Analyzer.Flags.BoolVar(&HelperPackages, "helper-packages", true, "also check packages named testutil or ending in \"test\"")
	Analyzer.Flags.BoolVar(&Eventually, "eventually", false, "also report testify Eventually, EventuallyWithT, and Never outside a bubble")
}

const eventuallyMessage = "%s in a test outside a synctest bubble polls the wall clock; wait on a channel or signal, or run the test under testing/synctest"

func run(pass *analysis.Pass) (any, error) {
	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	inBubble := bubbleChecker(pass, inspect)
	helperPackage := HelperPackages && isHelperPackage(pass.Pkg.Name())
	inspect.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node) {
		call := n.(*ast.CallExpr)
		if !helperPackage && !isTestFile(pass, call) {
			return
		}
		switch {
		case isPackageFunc(pass, call, "time", "Sleep"):
			if !inBubble(call) {
				pass.Reportf(call.Pos(), "%s", diagnosticMessage)
			}
		case Eventually && isTestifyFunc(pass, call, pollingAssertions...):
			if !inBubble(call) {
				pass.Reportf(call.Pos(), eventuallyMessage, calleeName(pass, call))
			}
		}
	})

	return nil, nil
}

// bubbleChecker reports whether a node lies inside a synctest bubble: a
// literal passed inline to synctest.Test, or the declaration or literal behind
// an identifier passed by name when every use of it is through synctest.Test.
func bubbleChecker(pass *analysis.Pass, inspect *inspector.Inspector) func(ast.Node) bool {
	bodies := functionBodies(pass)
	var bubbles []ast.Node
	uses := make(map[types.Object]int, len(bodies))
	for _, obj := range pass.TypesInfo.Uses {
		if _, ok := bodies[obj]; ok {
			uses[obj]++
		}
	}
	callbackUses := make(map[types.Object]int, len(bodies))
	inspect.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node) {
		call := n.(*ast.CallExpr)
		if !isPackageFunc(pass, call, "testing/synctest", "Test", "Run") {
			return
		}
		for _, arg := range call.Args {
			switch arg := ast.Unparen(arg).(type) {
			case *ast.FuncLit:
				bubbles = append(bubbles, arg)
			case *ast.Ident:
				if obj := pass.TypesInfo.Uses[arg]; bodies[obj] != nil {
					callbackUses[obj]++
				}
			}
		}
	})
	for obj, count := range callbackUses {
		if count == uses[obj] {
			bubbles = append(bubbles, bodies[obj])
		}
	}
	return func(n ast.Node) bool {
		for _, bubble := range bubbles {
			if n.Pos() >= bubble.Pos() && n.End() <= bubble.End() {
				return true
			}
		}
		return false
	}
}

// functionBodies maps each function declared in the package, and each
// variable initialized with a function literal, to the node holding its body.
func functionBodies(pass *analysis.Pass) map[types.Object]ast.Node {
	bodies := map[types.Object]ast.Node{}
	bind := func(name *ast.Ident, value ast.Expr) {
		if lit, ok := ast.Unparen(value).(*ast.FuncLit); ok {
			if obj := pass.TypesInfo.Defs[name]; obj != nil {
				bodies[obj] = lit
			}
		}
	}
	for _, file := range pass.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if obj := pass.TypesInfo.Defs[n.Name]; obj != nil && n.Body != nil {
					bodies[obj] = n.Body
				}
			case *ast.AssignStmt:
				if len(n.Lhs) == len(n.Rhs) {
					for i, lhs := range n.Lhs {
						if name, ok := lhs.(*ast.Ident); ok {
							bind(name, n.Rhs[i])
						}
					}
				}
			case *ast.ValueSpec:
				if len(n.Names) == len(n.Values) {
					for i, name := range n.Names {
						bind(name, n.Values[i])
					}
				}
			}
			return true
		})
	}
	return bodies
}

func isHelperPackage(name string) bool {
	name = strings.TrimSuffix(name, "_test")
	return name == "testutil" || strings.HasSuffix(name, "test")
}

var pollingAssertions = []string{"Eventually", "Eventuallyf", "EventuallyWithT", "EventuallyWithTf", "Never", "Neverf"}

func isTestifyFunc(pass *analysis.Pass, call *ast.CallExpr, names ...string) bool {
	for _, path := range []string{"github.com/stretchr/testify/assert", "github.com/stretchr/testify/require"} {
		if isPackageFunc(pass, call, path, names...) {
			return true
		}
	}
	return false
}

func calleeName(pass *analysis.Pass, call *ast.CallExpr) string {
	fn := typeutil.StaticCallee(pass.TypesInfo, call)
	return fn.Pkg().Name() + "." + fn.Name()
}

func isTestFile(pass *analysis.Pass, n ast.Node) bool {
	return strings.HasSuffix(pass.Fset.Position(n.Pos()).Filename, "_test.go")
}

func isPackageFunc(pass *analysis.Pass, call *ast.CallExpr, pkgPath string, names ...string) bool {
	// StaticCallee also resolves parenthesized callees, dot imports, and method expressions.
	fn := typeutil.StaticCallee(pass.TypesInfo, call)
	if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != pkgPath {
		return false
	}
	for _, name := range names {
		if fn.Name() == name {
			return true
		}
	}
	return false
}
