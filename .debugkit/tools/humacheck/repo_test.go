package humacheck

import (
	"slices"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mapFS(files map[string]string) (fstest.MapFS, []string) {
	fsys := fstest.MapFS{}
	var tracked []string
	for name, content := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(content)}
		tracked = append(tracked, name)
	}
	slices.Sort(tracked)
	return fsys, tracked
}

func specs(t *testing.T, fsys fstest.MapFS, tracked []string, hasAPI bool, goMod string) []Diagnostic {
	t.Helper()
	diags, err := checkSpecs(fsys, tracked, hasAPI, goMod)
	require.NoError(t, err)
	return diags
}

func generators(t *testing.T, fsys fstest.MapFS, tracked []string, hasAPI bool, goMod string) []Diagnostic {
	t.Helper()
	diags, err := checkGenerators(fsys, tracked, hasAPI, goMod)
	require.NoError(t, err)
	return diags
}

const (
	yamlSpec = "openapi: 3.1.0\ninfo:\n  title: t\n"
	jsonSpec = `{"openapi": "3.1.0", "info": {"title": "t"}}`
)

func TestCheckSpecs(t *testing.T) {
	t.Parallel()

	t.Run("json document flagged and yaml counted", func(t *testing.T) {
		t.Parallel()
		fsys, tracked := mapFS(map[string]string{
			"go.mod":                           "module m\n",
			"api/openapi.yaml":                 yamlSpec,
			"internal/client/openapi-3.0.json": jsonSpec,
			"config/settings.json":             `{"openapi": false}`,
			"docs/other.yml":                   "openapi: guide\n",
			"node_modules/dep/openapi.json":    jsonSpec,
			"testdata/fixture.json":            jsonSpec,
		})
		diags := specs(t, fsys, tracked, true, "go.mod")
		require.Len(t, diags, 1)
		assert.Equal(t, Diagnostic{Path: "internal/client/openapi-3.0.json", Line: 1, Column: 1, Rule: RuleSpec, Message: specJSONMessage}, diags[0])
	})

	t.Run("openapi key deep in the document is found", func(t *testing.T) {
		t.Parallel()
		fsys, tracked := mapFS(map[string]string{
			"go.mod":           "module m\n",
			"api/openapi.yaml": "components:\n  schemas:\n    A:\n      type: object\ninfo:\n  title: t\nopenapi: 3.1.0\n",
		})
		assert.Empty(t, specs(t, fsys, tracked, true, "go.mod"))
	})

	t.Run("missing document reported at go.mod only with an API", func(t *testing.T) {
		t.Parallel()
		assert := assert.New(t)
		fsys, tracked := mapFS(map[string]string{"go.mod": "module m\n"})
		diags := specs(t, fsys, tracked, true, "go.mod")
		require.Len(t, diags, 1)
		assert.Equal("go.mod", diags[0].Path)
		assert.Equal(specMissingMessage, diags[0].Message)
		assert.Empty(specs(t, fsys, tracked, false, "go.mod"))
	})

	t.Run("sibling module spec does not satisfy a nested module", func(t *testing.T) {
		t.Parallel()
		assert := assert.New(t)
		fsys, tracked := mapFS(map[string]string{
			"apps/api/go.mod":         "module m\n",
			"apps/other/openapi.yaml": yamlSpec,
		})
		diags := specs(t, fsys, tracked, true, "apps/api/go.mod")
		require.Len(t, diags, 1)
		assert.Equal("apps/api/go.mod", diags[0].Path)

		fsys["apps/api/api/openapi.yaml"] = &fstest.MapFile{Data: []byte(yamlSpec)}
		tracked = append(tracked, "apps/api/api/openapi.yaml")
		assert.Empty(specs(t, fsys, tracked, true, "apps/api/go.mod"))
	})

	t.Run("nested module spec does not satisfy the root module", func(t *testing.T) {
		t.Parallel()
		assert := assert.New(t)
		fsys, tracked := mapFS(map[string]string{
			"go.mod":                   "module m\n",
			"tools/gen/go.mod":         "module m/tools/gen\n",
			"tools/gen/openapi.yaml":   yamlSpec,
			"testdata/nested/go.mod":   "module ignored\n",
			"testdata/nested/api.yaml": yamlSpec,
		})
		diags := specs(t, fsys, tracked, true, "go.mod")
		require.Len(t, diags, 1)
		assert.Equal("go.mod", diags[0].Path)
		assert.Empty(specs(t, fsys, tracked, true, "tools/gen/go.mod"))
	})

	t.Run("unreadable tracked file fails the run", func(t *testing.T) {
		t.Parallel()
		fsys, _ := mapFS(map[string]string{"go.mod": "module m\n"})
		_, err := checkSpecs(fsys, []string{"go.mod", "api/openapi.yaml"}, true, "go.mod")
		require.ErrorContains(t, err, "api/openapi.yaml")
	})
}

func TestCheckGenerators(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		files    map[string]string
		hasAPI   bool
		wantMsgs []string
	}{
		{
			name:   "standard go generator in go.mod satisfies",
			files:  map[string]string{"go.mod": "module m\n\nrequire github.com/doordash-oss/oapi-codegen-dd/v3 v3.75.5\n"},
			hasAPI: true,
		},
		{
			name:   "orval in package.json satisfies",
			files:  map[string]string{"go.mod": "module m\n", "web/package.json": `{"devDependencies": {"orval": "8.26.0"}}`},
			hasAPI: true,
		},
		{
			name:   "unquoted command form in a Makefile satisfies",
			files:  map[string]string{"go.mod": "module m\n", "Makefile": "web-generate:\n\tcd web && npx orval --config orval.config.ts\n"},
			hasAPI: true,
		},
		{
			name:   "generate directive in an ordinary Go file satisfies",
			files:  map[string]string{"go.mod": "module m\n", "pkg/client/client.go": "package client\n\n//go:generate go run github.com/doordash-oss/oapi-codegen-dd/v3/cmd/oapi-codegen -config c.yaml spec.yaml\n"},
			hasAPI: true,
		},
		{
			name:     "generator named only in a comment does not satisfy",
			files:    map[string]string{"go.mod": "module m\n", "Makefile": "# TODO: adopt orval\nbuild:\n\tgo build ./...\n"},
			hasAPI:   true,
			wantMsgs: []string{generatorMissing},
		},
		{
			name:   "banned generator in a comment is not reported",
			files:  map[string]string{"go.mod": "module m\n\nrequire github.com/doordash-oss/oapi-codegen-dd/v3 v3.75.5\n", "scripts/gen.sh": "# we no longer use swagger-codegen\nnpx orval\n"},
			hasAPI: true,
		},
		{
			name:     "no generator with API",
			files:    map[string]string{"go.mod": "module m\n"},
			hasAPI:   true,
			wantMsgs: []string{generatorMissing},
		},
		{
			name:   "no generator without API",
			files:  map[string]string{"go.mod": "module m\n"},
			hasAPI: false,
		},
		{
			name: "nonstandard generators reported per generator and do not satisfy",
			files: map[string]string{
				"go.mod":           "module m\n\nrequire github.com/oapi-codegen/oapi-codegen/v2 v2.8.0\n",
				"web/package.json": `{"devDependencies": {"openapi-typescript": "7.13.0", "openapi-fetch": "0.17.0"}}`,
			},
			hasAPI:   true,
			wantMsgs: []string{nonstandardGenerators[0].message, nonstandardGenerators[3].message, nonstandardGenerators[4].message, generatorMissing},
		},
		{
			name:   "indirect requirement is not a choice",
			files:  map[string]string{"go.mod": "module m\n\nrequire (\n\tgithub.com/doordash-oss/oapi-codegen-dd/v3 v3.75.5\n\tgithub.com/oapi-codegen/oapi-codegen/v2 v2.8.0 // indirect\n)\n"},
			hasAPI: true,
		},
		{
			name:     "indirect marker outside go.mod does not suppress",
			files:    map[string]string{"go.mod": "module m\n\nrequire github.com/doordash-oss/oapi-codegen-dd/v3 v3.75.5\n", "scripts/gen.mjs": "run('openapi-typescript', spec); // indirect\n"},
			hasAPI:   true,
			wantMsgs: []string{nonstandardGenerators[3].message},
		},
		{
			name:   "longer names containing a generator name are not matched",
			files:  map[string]string{"go.mod": "module m\n\nrequire github.com/doordash-oss/oapi-codegen-dd/v3 v3.75.5\n", "web/package.json": `{"devDependencies": {"openapi-typescript-helpers": "1.0.0"}}`},
			hasAPI: true,
		},
		{
			name: "banned generators flagged",
			files: map[string]string{
				"go.mod":           "module m\n\nrequire github.com/deepmap/oapi-codegen v1.16.0\n",
				"web/package.json": `{"devDependencies": {"@openapitools/openapi-generator-cli": "2.0.0", "orval": "8.26.0"}}`,
			},
			hasAPI:   true,
			wantMsgs: []string{bannedGenerators[0].message, bannedGenerators[2].message},
		},
		{
			name:   "generated file header from oapi-codegen-dd counts as the standard generator",
			files:  map[string]string{"go.mod": "module m\n", "pkg/client/client.gen.go": "// Package client provides primitives to interact with the openapi HTTP API.\n//\n// Code generated by oapi-codegen. DO NOT EDIT.\npackage client\n"},
			hasAPI: true,
		},
		{
			name:     "generated file header from oapi-codegen v2 is reported at the file",
			files:    map[string]string{"go.mod": "module m\n\nrequire github.com/doordash-oss/oapi-codegen-dd/v3 v3.75.5\n", "internal/api/gen.go": "// Package api provides primitives to interact with the openapi HTTP API.\n//\n// Code generated by github.com/oapi-codegen/oapi-codegen/v2 version v2.4.1 DO NOT EDIT.\npackage api\n"},
			hasAPI:   true,
			wantMsgs: []string{nonstandardGenerators[0].message},
		},
		{
			name:     "generated file header from ogen is reported",
			files:    map[string]string{"go.mod": "module m\n\nrequire github.com/doordash-oss/oapi-codegen-dd/v3 v3.75.5\n", "internal/api/oas_client_gen.go": "// Code generated by ogen, DO NOT EDIT.\n\npackage api\n"},
			hasAPI:   true,
			wantMsgs: []string{nonstandardGenerators[2].message},
		},
		{
			name:     "generate directive inside a generated file is ignored",
			files:    map[string]string{"go.mod": "module m\n", "pkg/x/x.gen.go": "// Code generated by sqlc. DO NOT EDIT.\n\npackage x\n\n//go:generate go run github.com/doordash-oss/oapi-codegen-dd/v3/cmd/oapi-codegen -config c.yaml spec.yaml\n"},
			hasAPI:   true,
			wantMsgs: []string{generatorMissing},
		},
		{
			name:     "generate directive under a generated directory is ignored",
			files:    map[string]string{"go.mod": "module m\n", "pkg/generated/x.go": "package generated\n\n//go:generate go run github.com/doordash-oss/oapi-codegen-dd/v3/cmd/oapi-codegen -config c.yaml spec.yaml\n"},
			hasAPI:   true,
			wantMsgs: []string{generatorMissing},
		},
		{
			name:   "node_modules ignored",
			files:  map[string]string{"go.mod": "module m\n\nrequire github.com/doordash-oss/oapi-codegen-dd/v3 v3.75.5\n", "web/node_modules/x/package.json": `{"name": "swagger-typescript-api"}`},
			hasAPI: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys, tracked := mapFS(tt.files)
			diags := generators(t, fsys, tracked, tt.hasAPI, "go.mod")
			var msgs []string
			for _, d := range diags {
				assert.Equal(t, RuleGenerator, d.Rule)
				msgs = append(msgs, d.Message)
			}
			assert.ElementsMatch(t, tt.wantMsgs, msgs)
		})
	}
}

func TestCheckFrontend(t *testing.T) {
	t.Parallel()
	routes := testRoutes("/ping", "/accounts/{id}", "/jobs", "/queue")
	fsys, tracked := mapFS(map[string]string{
		"frontend/src/lib/api/client.ts": "" +
			"import axios from 'axios';\n" + // an import mentioning axios is not a call
			"export async function ping(base: string) {\n" +
			"  const res = await fetch(`${base}/api/v1/ping`, { headers: { 'X-Note': '/api/v1/queue' } });\n" + // options object is not the URL
			"  return res.json();\n" +
			"}\n" +
			"export const account = (id: string) =>\n" +
			"  fetch(\n" +
			"    '/api/v1/accounts/' + id,\n" + // multi-line first argument
			"  );\n" +
			"// fetch('/api/v1/jobs') in a comment is ignored\n" +
			"export const link = '/api/v1/jobs';\n" + // no call
			"const later = axios.get('/api/v1/jobs');\n" +
			"/**\n" +
			" * Example: fetch('/api/v1/queue')\n" + // block comment
			" */\n" +
			"const doc = 'call fetch(\"/api/v1/queue\") first'; // fetch('/api/v1/queue') trailing\n" + // string and trailing comment
			"const real = fetch('/api/v1/queue'); /* fetch('/api/v1/ping') */\n",
		"frontend/src/lib/api/generated/client.ts": "fetch('/api/v1/ping');\n",
		"frontend/src/lib/api/client.test.ts":      "fetch('/api/v1/ping');\n",
		"frontend/node_modules/x/index.js":         "fetch('/api/v1/ping');\n",
		"frontend/src/App.svelte":                  "<script>\n  const r = fetch(\"/api/v1/jobs\");\n</script>\n",
	})
	diags, err := checkFrontend(fsys, tracked, routes)
	require.NoError(t, err)
	var got []string
	for _, d := range diags {
		assert.Equal(t, RuleFrontend, d.Rule)
		got = append(got, d.String())
	}
	assert.Equal(t, []string{
		"frontend/src/App.svelte:2:19: " + formatClientMessage("/api/v1/jobs") + " (frontend)",
		"frontend/src/lib/api/client.ts:3:27: " + formatClientMessage("/api/v1/ping") + " (frontend)",
		"frontend/src/lib/api/client.ts:8:5: " + formatClientMessage("/api/v1/accounts/{param}") + " (frontend)",
		"frontend/src/lib/api/client.ts:12:25: " + formatClientMessage("/api/v1/jobs") + " (frontend)",
		"frontend/src/lib/api/client.ts:17:20: " + formatClientMessage("/api/v1/queue") + " (frontend)",
	}, got)
}

func TestMaskSource(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	src := "a('/x'); // c('/y')\n/* d('/z') */ e(`/${w}`)\nf(\"g('/h')\")"
	code, calls := maskSource(src)
	assert.Len(code, len(src))
	assert.Len(calls, len(src))
	assert.Equal("a('/x');           \n              e(`/${w}`)\nf(\"g('/h')\")", code)
	assert.Equal("a('  ');           \n              e(`     `)\nf(\"       \")", calls)
}

func TestFirstArgumentEnd(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	end, ok := firstArgumentEnd(`"a,b", {x: 1})`, 0)
	require.True(t, ok)
	assert.Equal(`"a,b"`, `"a,b", {x: 1})`[:end])
	end, ok = firstArgumentEnd("`${f(a, b)}/x`)", 0)
	require.True(t, ok)
	assert.Equal("`${f(a, b)}/x`", "`${f(a, b)}/x`)"[:end])
	_, ok = firstArgumentEnd(`"unterminated`, 0)
	assert.False(ok)
}

func TestFrontendSourceFile(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	assert.True(frontendSourceFile("web/src/lib/api.ts"))
	assert.True(frontendSourceFile("web/src/App.svelte"))
	assert.False(frontendSourceFile("web/src/lib/api.d.ts"))
	assert.False(frontendSourceFile("web/src/lib/api.test.ts"))
	assert.False(frontendSourceFile("web/src/lib/generated/api.ts"))
	assert.False(frontendSourceFile("web/e2e/smoke.ts"))
	assert.False(frontendSourceFile("web/scripts/e2e.ts"))
	assert.False(frontendSourceFile("web/src/lib/api.go"))
}
