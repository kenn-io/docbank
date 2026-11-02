package sqlownership

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/ctrlflow"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/cfg"
)

// everyPath reports whether every path from the statement that assigns the
// call's resource accounts for that resource: the path returns it, hands it to
// other code, or closes it. Like go vet's lostcancel, it searches the
// control-flow graph for one path to a return that does not.
//
// A resource that is never held in a variable has no such statement, and the
// SSA proof stands alone.
func everyPath(pass *analysis.Pass, call token.Pos) bool {
	o := locate(pass, call)
	if o == nil {
		return true
	}
	cfgs := pass.ResultOf[ctrlflow.Analyzer].(*ctrlflow.CFGs)
	var graph *cfg.CFG
	switch fn := o.function.(type) {
	case *ast.FuncDecl:
		graph = cfgs.FuncDecl(fn)
	case *ast.FuncLit:
		graph = cfgs.FuncLit(fn)
	}
	if graph == nil {
		return false
	}
	for _, block := range graph.Blocks {
		for i, node := range block.Nodes {
			if node == o.definition {
				return !o.leaks(block, block.Nodes[i+1:], map[*cfg.Block]bool{})
			}
		}
	}
	return false
}

// owner is one variable that holds a resource, and where it was assigned.
type owner struct {
	info        *types.Info
	definition  ast.Node // *ast.AssignStmt or *ast.ValueSpec
	function    ast.Node // enclosing *ast.FuncDecl or *ast.FuncLit
	resource    *types.Var
	err         *types.Var // error assigned by the same statement, if any
	namedResult bool
}

func locate(pass *analysis.Pass, call token.Pos) *owner {
	for _, file := range pass.Files {
		if call < file.FileStart || call >= file.FileEnd {
			continue
		}
		path, _ := astutil.PathEnclosingInterval(file, call, call)
		o := &owner{info: pass.TypesInfo}
		for i, node := range path {
			expr, ok := node.(*ast.CallExpr)
			if !ok || expr.Lparen != call || i+1 >= len(path) {
				continue
			}
			var names []ast.Expr
			switch stmt := path[i+1].(type) {
			case *ast.AssignStmt:
				if len(stmt.Rhs) == 1 {
					names = stmt.Lhs
				}
			case *ast.ValueSpec:
				if len(stmt.Values) == 1 {
					for _, name := range stmt.Names {
						names = append(names, name)
					}
				}
			}
			for _, name := range names {
				v := o.variable(name)
				switch {
				case v == nil:
				case resource(v.Type()):
					o.resource = v
				case types.Identical(v.Type(), types.Universe.Lookup("error").Type()):
					o.err = v
				}
			}
			if o.resource == nil {
				return nil
			}
			o.definition = path[i+1]
			for _, outer := range path[i+1:] {
				switch fn := outer.(type) {
				case *ast.FuncDecl:
					o.function, o.namedResult = fn, o.declares(fn.Type.Results)
				case *ast.FuncLit:
					o.function, o.namedResult = fn, o.declares(fn.Type.Results)
				}
				if o.function != nil {
					return o
				}
			}
		}
	}
	return nil
}

func (o *owner) variable(expr ast.Expr) *types.Var {
	id, ok := expr.(*ast.Ident)
	if !ok {
		return nil
	}
	if v, ok := o.info.Defs[id].(*types.Var); ok {
		return v
	}
	v, _ := o.info.Uses[id].(*types.Var)
	return v
}

func (o *owner) declares(results *ast.FieldList) bool {
	if results == nil {
		return false
	}
	for _, field := range results.List {
		for _, name := range field.Names {
			if o.info.Defs[name] == o.resource {
				return true
			}
		}
	}
	return false
}

// leaks searches depth-first for a path on which the resource is replaced or
// the function returns before the resource is accounted for.
func (o *owner) leaks(block *cfg.Block, nodes []ast.Node, seen map[*cfg.Block]bool) bool {
	for _, node := range nodes {
		switch stmt := node.(type) {
		case *ast.ReturnStmt:
			return !o.mentioned(stmt) && (len(stmt.Results) > 0 || !o.namedResult)
		case *ast.AssignStmt:
			if o.mentioned(stmt) {
				return false
			}
			for _, lhs := range stmt.Lhs {
				if o.variable(lhs) == o.resource {
					return true
				}
			}
		default:
			if o.mentioned(node) {
				return false
			}
		}
	}
	for _, next := range block.Succs {
		if seen[next] || o.failed(block, next) {
			continue
		}
		seen[next] = true
		if o.leaks(next, next.Nodes, seen) {
			return true
		}
	}
	return false
}

// failed reports whether the edge from one block to the next is taken only
// when the statement that produced the resource also produced an error. The
// resource is nil there. It covers both "if err != nil" and "if err == nil".
func (o *owner) failed(from, to *cfg.Block) bool {
	if o.err == nil {
		return false
	}
	// Only the block that evaluates an if condition has a then-successor.
	var then *cfg.Block
	for _, succ := range from.Succs {
		if succ.Kind == cfg.KindIfThen {
			then = succ
		}
	}
	if then == nil {
		return false
	}
	cond, ok := then.Stmt.(*ast.IfStmt).Cond.(*ast.BinaryExpr)
	if !ok {
		return false
	}
	isNil := func(expr ast.Expr) bool {
		id, ok := expr.(*ast.Ident)
		return ok && o.info.Uses[id] == types.Universe.Lookup("nil")
	}
	onErr := o.variable(cond.X) == o.err && isNil(cond.Y) || o.variable(cond.Y) == o.err && isNil(cond.X)
	if !onErr {
		return false
	}
	if cond.Op == token.NEQ {
		return to == then
	}
	return cond.Op == token.EQL && to != then
}

// mentioned reports whether node returns, stores, passes, or closes the
// resource. Calling any other method on it, or assigning to it, does not
// account for it.
func (o *owner) mentioned(node ast.Node) bool {
	found := false
	var visit func(ast.Node) bool
	visit = func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				if _, plain := lhs.(*ast.Ident); !plain {
					ast.Inspect(lhs, visit)
				}
			}
			for _, rhs := range n.Rhs {
				ast.Inspect(rhs, visit)
			}
			return false
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && o.variable(sel.X) == o.resource {
				found = found || sel.Sel.Name == "Close"
				for _, arg := range n.Args {
					ast.Inspect(arg, visit)
				}
				return false
			}
		case *ast.Ident:
			found = found || o.info.Uses[n] == o.resource
		}
		return !found
	}
	ast.Inspect(node, visit)
	return found
}
