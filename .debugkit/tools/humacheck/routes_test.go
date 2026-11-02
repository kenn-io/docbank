package humacheck

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func testRoutes(paths ...string) *routeSet {
	rs := &routeSet{adapterPrefixes: []string{"", "/api/v1"}, paths: paths}
	for _, prefix := range rs.adapterPrefixes {
		for _, path := range rs.paths {
			rs.concrete = append(rs.concrete, prefix+path)
		}
	}
	return rs
}

func TestRouteMatch(t *testing.T) {
	t.Parallel()
	routes := testRoutes("/ping", "/accounts/{id}", "/jobs", "/jobs/{id}/review", "/api/health", "/files/{path...}")

	tests := []struct {
		name    string
		literal string
		want    string
		matched bool
	}{
		{name: "exact with prefix", literal: "/api/v1/ping", want: "/api/v1/ping", matched: true},
		{name: "exact without prefix", literal: "/ping", want: "/ping", matched: true},
		{name: "absolute url", literal: "http://kata.invalid/api/v1/ping", want: "/api/v1/ping", matched: true},
		{name: "format verb host", literal: "%s/api/v1/jobs?id=%d", want: "/api/v1/jobs", matched: true},
		{name: "verb as param", literal: "/api/v1/jobs/%d/review", want: "/api/v1/jobs/{param}/review", matched: true},
		{name: "trailing slash awaits id", literal: "/api/v1/accounts/", want: "/api/v1/accounts/{param}", matched: true},
		{name: "template hole", literal: "%s/api/v1/accounts/%s", want: "/api/v1/accounts/{param}", matched: true},
		{name: "unprefixed route literal", literal: "/api/health", want: "/api/health", matched: true},
		{name: "foreign path", literal: "/api/tags", matched: false},
		{name: "partial parent", literal: "/api/v1/accounts", matched: false},
		{name: "root only", literal: "/", matched: false},
		{name: "no path", literal: "GET", matched: false},
		{name: "prose with path", literal: "fetch /api/v1/ping failed", matched: false},
		{name: "double slash", literal: "//api/v1/ping", matched: false},
		{name: "placeholder only", literal: "%s/%s", matched: false},
		{name: "concrete parameter value", literal: "/api/v1/accounts/42", want: "/api/v1/accounts/{param}", matched: true},
		{name: "wildcard tail", literal: "/api/v1/files/a/b/c", want: "/api/v1/files/{param}", matched: true},
		{name: "wildcard tail needs a segment", literal: "/api/v1/files", matched: false},
		{name: "param only", literal: "/{id}", matched: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := routes.match(tt.literal)
			assert.Equal(t, tt.matched, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestExtractPath(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"/api/v1/ping":                   "/api/v1/ping",
		"https://example.com/x?y=1#frag": "/x",
		"%s/api/jobs?id=%d":              "/api/jobs",
		"no path here":                   "",
		"host:8080/health":               "/health",
	}
	for literal, want := range tests {
		assert.Equal(t, want, extractPath(literal), literal)
	}
}
