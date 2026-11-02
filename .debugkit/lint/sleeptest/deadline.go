package sleeptest

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"strings"
	"time"
	"unicode"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/types/typeutil"
)

// DeadlineAnalyzer reports sub-second wall-clock budgets in _test.go files
// outside synctest bubbles.
var DeadlineAnalyzer = &analysis.Analyzer{
	Name:     "deadlinetest",
	Doc:      "reports sub-second named time budgets, deadlines, timers, and testify polling budgets in test files outside a testing/synctest bubble",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      runDeadline,
}

const deadlineMessage = "%s with budget %s in a test outside a synctest bubble races the wall clock; wait on a real event, or run the test under testing/synctest"

func runDeadline(pass *analysis.Pass) (any, error) {
	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	inBubble := bubbleChecker(pass, inspect)
	report := func(node ast.Node, arg ast.Expr, name string, kind budgetKind) {
		if budget, ok := constantDuration(pass, arg); ok && kind.reports(budget) {
			pass.Reportf(node.Pos(), deadlineMessage, name, budget)
		}
	}
	// WithDeadline owns its inline Add diagnostic.
	deadlines := map[ast.Node]bool{}
	inspect.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node) {
		call := n.(*ast.CallExpr)
		if isPackageFunc(pass, call, "context", "WithDeadline", "WithDeadlineCause") && nowPlus(pass, argAt(call, 1)) != nil {
			deadlines[ast.Unparen(argAt(call, 1))] = true
		}
	})
	inspect.Preorder([]ast.Node{(*ast.CallExpr)(nil), (*ast.CompositeLit)(nil), (*ast.AssignStmt)(nil)}, func(n ast.Node) {
		if !isTestFile(pass, n) || inBubble(n) {
			return
		}
		switch n := n.(type) {
		case *ast.CallExpr:
			if arg, kind := budgetArg(pass, n); arg != nil {
				report(n, arg, calleeName(pass, n), kind)
				return
			}
			if arg := nowPlus(pass, n); arg != nil {
				if !deadlines[n] {
					report(n, arg, "time.Now().Add", timerBudget)
				}
				return
			}
			fn := typeutil.StaticCallee(pass.TypesInfo, n)
			if fn == nil || fn.Pkg() == nil {
				return
			}
			path := fn.Pkg().Path()
			if path != pass.Pkg.Path() && (pass.Module == nil || (path != pass.Module.Path && !strings.HasPrefix(path, pass.Module.Path+"/"))) {
				return
			}
			sig, ok := pass.TypesInfo.TypeOf(n.Fun).(*types.Signature)
			if !ok {
				return
			}
			for i, arg := range n.Args {
				param := i
				if sig.Variadic() && param >= sig.Params().Len()-1 {
					if n.Ellipsis.IsValid() {
						break
					}
					param = sig.Params().Len() - 1
					if budgetName(sig.Params().At(param).Name()) && isDuration(sig.Params().At(param).Type().(*types.Slice).Elem()) {
						report(arg, arg, fn.Name(), timerBudget)
					}
				} else if param < sig.Params().Len() && budgetName(sig.Params().At(param).Name()) && isDuration(sig.Params().At(param).Type()) {
					report(arg, arg, fn.Name(), timerBudget)
				}
			}
		case *ast.CompositeLit:
			typ := pass.TypesInfo.TypeOf(n)
			if typ == nil {
				return
			}
			if ptr, ok := typ.Underlying().(*types.Pointer); ok {
				typ = ptr.Elem()
			}
			fields, ok := typ.Underlying().(*types.Struct)
			if !ok {
				return
			}
			for i, elt := range n.Elts {
				value := elt
				var field *types.Var
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					key, ok := kv.Key.(*ast.Ident)
					if !ok {
						continue
					}
					field, _ = pass.TypesInfo.Uses[key].(*types.Var)
					value = kv.Value
				} else if i < fields.NumFields() {
					field = fields.Field(i)
				}
				if field != nil && budgetName(field.Name()) && isDuration(field.Type()) {
					report(value, value, field.Name(), timerBudget)
				}
			}
		case *ast.AssignStmt:
			if n.Tok != token.ASSIGN || len(n.Lhs) != len(n.Rhs) {
				return
			}
			for i, lhs := range n.Lhs {
				sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr)
				if !ok {
					continue
				}
				field, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Var)
				if ok && field.IsField() && budgetName(field.Name()) && isDuration(field.Type()) {
					report(n.Rhs[i], n.Rhs[i], field.Name(), timerBudget)
				}
			}
		}
	})
	return nil, nil
}

// budgetKind says which constant budgets of a wall-clock call are reported.
type budgetKind int

const (
	// timerBudget reports positive sub-second budgets; a zero or negative
	// timer fires at once, so its outcome does not depend on timing.
	timerBudget budgetKind = iota
	// pollingBudget reports every sub-second budget; a zero one still passes
	// or fails by timing.
	pollingBudget
	// absenceBudget reports zero and negative budgets, which pass vacuously.
	// A short positive one checks fewer times under load but cannot fail.
	absenceBudget
)

func (k budgetKind) reports(budget time.Duration) bool {
	switch k {
	case timerBudget:
		return budget > 0 && budget < time.Second
	case pollingBudget:
		return budget < time.Second
	case absenceBudget:
		return budget <= 0
	}
	return false
}

// budgetArg returns the duration argument of a call that waits on the wall
// clock, or nil when the call is not one of those, and which of its budgets
// are reported.
func budgetArg(pass *analysis.Pass, call *ast.CallExpr) (ast.Expr, budgetKind) {
	switch {
	case isPackageFunc(pass, call, "context", "WithTimeout", "WithTimeoutCause"):
		return argAt(call, 1), timerBudget
	case isPackageFunc(pass, call, "context", "WithDeadline", "WithDeadlineCause"):
		return nowPlus(pass, argAt(call, 1)), timerBudget
	case isPackageFunc(pass, call, "time", "After", "NewTimer", "AfterFunc"):
		return argAt(call, 0), timerBudget
	case isTestifyFunc(pass, call, "Never", "Neverf"):
		return argAt(call, waitForIndex(pass, call)), absenceBudget
	case isTestifyFunc(pass, call, pollingAssertions...):
		return argAt(call, waitForIndex(pass, call)), pollingBudget
	}
	return nil, timerBudget
}

// nowPlus returns d when expr is written directly as time.Now().Add(d).
func nowPlus(pass *analysis.Pass, expr ast.Expr) ast.Expr {
	add, ok := ast.Unparen(expr).(*ast.CallExpr)
	if !ok {
		return nil
	}
	if !isPackageFunc(pass, add, "time", "Add") {
		return nil
	}
	sel, ok := ast.Unparen(add.Fun).(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	now, ok := ast.Unparen(sel.X).(*ast.CallExpr)
	if !ok || !isPackageFunc(pass, now, "time", "Now") {
		return nil
	}
	return argAt(add, 0)
}

// waitForIndex finds the waitFor parameter by name in the call's own
// signature, so package functions, *Assertions methods, and method
// expressions, which differ by a leading t or receiver, resolve alike.
func waitForIndex(pass *analysis.Pass, call *ast.CallExpr) int {
	sig, ok := pass.TypesInfo.TypeOf(call.Fun).(*types.Signature)
	if !ok {
		return -1
	}
	params := sig.Params()
	for i := range params.Len() {
		if params.At(i).Name() == "waitFor" {
			return i
		}
	}
	return -1
}

func argAt(call *ast.CallExpr, i int) ast.Expr {
	if i < 0 || i >= len(call.Args) {
		return nil
	}
	return call.Args[i]
}

// budgetName limits handoffs to names with a camelCase or snake_case word,
// singular or plural, that names a time budget. Polling intervals, ticks,
// and retry delays are cadences: a short one speeds a test up and cannot
// fail it.
func budgetName(name string) bool {
	runes := []rune(name)
	start := 0
	for i := 1; i <= len(runes); i++ {
		if i < len(runes) && !wordBoundary(runes, i) {
			continue
		}
		word := strings.ToLower(strings.Trim(string(runes[start:i]), "_"))
		switch strings.TrimSuffix(word, "s") {
		case "timeout", "deadline", "budget", "wait":
			return true
		}
		start = i
	}
	return false
}

// wordBoundary reports whether a new word starts at runes[i]: after an
// underscore, at a lower-to-upper change, or at the last capital of an
// acronym followed by a lowercase letter, as in HTTPTimeout.
func wordBoundary(runes []rune, i int) bool {
	prev, cur := runes[i-1], runes[i]
	switch {
	case prev == '_' || cur == '_':
		return true
	case unicode.IsUpper(cur) && !unicode.IsUpper(prev):
		return true
	case unicode.IsUpper(cur) && i+1 < len(runes) && unicode.IsLower(runes[i+1]):
		return true
	}
	return false
}

func isDuration(typ types.Type) bool {
	named, ok := types.Unalias(typ).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "time" && named.Obj().Name() == "Duration"
}

func constantDuration(pass *analysis.Pass, expr ast.Expr) (time.Duration, bool) {
	if expr == nil {
		return 0, false
	}
	value := pass.TypesInfo.Types[expr].Value
	if value == nil {
		return 0, false
	}
	n, exact := constant.Int64Val(constant.ToInt(value))
	return time.Duration(n), exact
}
