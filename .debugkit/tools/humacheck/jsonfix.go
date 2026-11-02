package humacheck

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"

	"go.kenn.io/kit/atomicfile"
	"golang.org/x/tools/go/ast/astutil"
)

const jsonV1FixedMessage = "rewrote the encoding/json (v1) import to encoding/json/v2 and its one-to-one call sites; stage the file and fix any remaining compile errors by hand"

// rewriteJSONV1 switches every encoding/json import in src to
// encoding/json/v2 and rewrites the call shapes that map one-to-one onto
// v2: NewEncoder(w).Encode(v) becomes MarshalWrite(w, v),
// NewDecoder(r).Decode(v) becomes UnmarshalRead(r, v), and
// MarshalIndent(v, prefix, indent) becomes Marshal with jsontext indent
// options. Anything else the file used from v1 is left for the compiler to
// point at: the user chose a blind swap over leaving files on v1. changed
// is false when the file does not import encoding/json.
func rewriteJSONV1(filename string, src []byte) (out []byte, changed bool, err error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return nil, false, err
	}
	var names []string
	for _, imp := range file.Imports {
		if importPath(imp) != jsonV1Path {
			continue
		}
		imp.Path.Value = strconv.Quote(jsonV2Path)
		name := "json"
		if imp.Name != nil {
			name = imp.Name.Name
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, false, nil
	}
	isJSON := func(expr ast.Expr, method string) bool {
		sel, ok := ast.Unparen(expr).(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != method {
			return false
		}
		ident, ok := sel.X.(*ast.Ident)
		return ok && slices.Contains(names, ident.Name)
	}
	needJSONText := false
	astutil.Apply(file, nil, func(c *astutil.Cursor) bool {
		call, ok := c.Node().(*ast.CallExpr)
		if !ok {
			return true
		}
		// json.NewEncoder(w).Encode(v) / json.NewDecoder(r).Decode(v)
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && len(call.Args) == 1 {
			inner, ok := ast.Unparen(sel.X).(*ast.CallExpr)
			if ok && len(inner.Args) == 1 {
				pkgIdent := func() ast.Expr { return ast.Unparen(inner.Fun).(*ast.SelectorExpr).X }
				switch {
				case sel.Sel.Name == "Encode" && isJSON(inner.Fun, "NewEncoder"):
					c.Replace(&ast.CallExpr{
						Fun:  &ast.SelectorExpr{X: pkgIdent(), Sel: ast.NewIdent("MarshalWrite")},
						Args: []ast.Expr{inner.Args[0], call.Args[0]},
					})
				case sel.Sel.Name == "Decode" && isJSON(inner.Fun, "NewDecoder"):
					c.Replace(&ast.CallExpr{
						Fun:  &ast.SelectorExpr{X: pkgIdent(), Sel: ast.NewIdent("UnmarshalRead")},
						Args: []ast.Expr{inner.Args[0], call.Args[0]},
					})
				}
			}
			return true
		}
		// json.MarshalIndent(v, prefix, indent)
		if isJSON(call.Fun, "MarshalIndent") && len(call.Args) == 3 {
			needJSONText = true
			args := []ast.Expr{call.Args[0], &ast.CallExpr{
				Fun:  &ast.SelectorExpr{X: ast.NewIdent("jsontext"), Sel: ast.NewIdent("WithIndent")},
				Args: []ast.Expr{call.Args[2]},
			}}
			if lit, ok := call.Args[1].(*ast.BasicLit); !ok || lit.Value != `""` {
				args = append(args, &ast.CallExpr{
					Fun:  &ast.SelectorExpr{X: ast.NewIdent("jsontext"), Sel: ast.NewIdent("WithIndentPrefix")},
					Args: []ast.Expr{call.Args[1]},
				})
			}
			c.Replace(&ast.CallExpr{
				Fun:  &ast.SelectorExpr{X: ast.Unparen(call.Fun).(*ast.SelectorExpr).X, Sel: ast.NewIdent("Marshal")},
				Args: args,
			})
		}
		return true
	})
	if needJSONText {
		astutil.AddImport(fset, file, jsonTextPath)
	}
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, file); err != nil {
		return nil, false, fmt.Errorf("format %s: %w", filename, err)
	}
	return buf.Bytes(), true, nil
}

// rewriteJSONV1File rewrites one working-tree file in place, keeping its
// permissions. The file is replaced atomically, so a failed write never
// leaves a truncated source file; a symlink at path is refused rather than
// replaced or written through. It reports false when the file has no v1
// import.
func rewriteJSONV1File(path string) (bool, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	out, changed, err := rewriteJSONV1(path, src)
	if err != nil || !changed {
		return false, err
	}
	if err := writeAtomicFile(path, out, atomicfile.WithPreserveMode()); err != nil {
		return errors.Is(err, atomicfile.ErrPublished), err
	}
	return true, nil
}

var writeAtomicFile = atomicfile.WriteFile

// applyJSONFixes rewrites every file with a v1 import finding and collapses
// that file's import findings into one "fixed" finding, so the run still
// exits non-zero and the hook user restages the rewritten file. The
// finding comes from the index while the fix lands in the working tree;
// when the working-tree copy is missing or does not parse, the finding is
// kept as reported and the run continues.
func applyJSONFixes(root string, diags []Diagnostic) []Diagnostic {
	fixed := map[string]bool{}
	var kept []Diagnostic
	for _, d := range diags {
		if d.Rule != RuleJSONV2 || d.Message != jsonV1ImportMessage {
			kept = append(kept, d)
			continue
		}
		if fixed[d.Path] {
			continue
		}
		changed, err := rewriteJSONV1File(joinRepoPath(root, d.Path))
		if (err != nil && !errors.Is(err, atomicfile.ErrPublished)) || !changed {
			kept = append(kept, d)
			continue
		}
		fixed[d.Path] = true
		d.Message = jsonV1FixedMessage
		kept = append(kept, d)
	}
	return kept
}
