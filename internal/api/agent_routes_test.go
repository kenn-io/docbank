package api_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/docbank/document/agentops"
	"go.kenn.io/docbank/internal/agentapi"
)

func TestOperationRegistryCurrentRouteCensus(t *testing.T) {
	_, fixture := newTestServer(t, nil)
	actual := make([]string, 0)
	for path, item := range fixture.Server.API().OpenAPI().Paths {
		for method, operation := range map[string]*huma.Operation{
			"GET": item.Get, "PUT": item.Put, "POST": item.Post, "DELETE": item.Delete,
			"OPTIONS": item.Options, "HEAD": item.Head, "PATCH": item.Patch, "TRACE": item.Trace,
		} {
			if operation != nil && strings.HasPrefix(path, "/api/v1/") {
				actual = append(actual, method+" "+path)
			}
		}
	}
	actual = append(actual, rawAgentRoutePatterns(t)...)
	slices.Sort(actual)
	actual = slices.Compact(actual)
	for _, mustInclude := range []string{
		"POST /api/v1/uploads", "PUT /api/v1/nodes/{id}/content", "GET /api/v1/jobs",
		"GET /api/v1/formats/capabilities",
	} {
		if !slices.Contains(actual, mustInclude) {
			t.Fatalf("actual route census omitted %s", mustInclude)
		}
	}
	if err := agentapi.ValidateCensus(actual, agentops.CurrentRoutes()); err != nil {
		t.Fatal(err)
	}
}

// rawAgentRoutePatterns reads the registration calls in this package so an
// added plain ServeMux route enters the census even before Huma documents it.
func rawAgentRoutePatterns(t *testing.T) []string {
	t.Helper()
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var patterns []string
	for _, file := range files {
		name := file.Name()
		if file.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (selector.Sel.Name != "Handle" && selector.Sel.Name != "HandleFunc") {
				return true
			}
			receiver, ok := selector.X.(*ast.Ident)
			if !ok || receiver.Name != "mux" {
				return true
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			pattern, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(pattern, " /api/v1/") {
				patterns = append(patterns, pattern)
			}
			return true
		})
	}
	return patterns
}
