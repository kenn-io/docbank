package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fixtureImports = `import ("testing"; "time"; "github.com/stretchr/testify/assert"; "github.com/stretchr/testify/require")`

func fixtureSource(imports, function, body string) string {
	return "package fixture\n" + imports + "\nfunc " + function + "(t *testing.T) {\n" + body + "\n}\n"
}

func fixtureDiagnostic(path string, line int, assertion, duration string) string {
	return fmt.Sprintf("%s:%d: github.com/stretchr/testify/%s budget %s is below 1s; synchronize in-process work or justify a retained integration budget in allowedBudgets\n", path, line, assertion, duration)
}

func writeTimingFixtures(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for path, source := range files {
		fullPath := filepath.Join(root, filepath.FromSlash(path))
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755))
		require.NoError(t, os.WriteFile(fullPath, []byte(source), 0o644))
	}
}

func assertTimingScan(t *testing.T, root string, files map[string]string, wantCode int, wantOutput string) {
	t.Helper()
	var stderr bytes.Buffer
	assert.Equal(t, wantCode, run([]string{root}, &stderr))
	assert.Equal(t, wantOutput, stderr.String())
	for path, source := range files {
		after, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		require.NoError(t, err)
		assert.Equal(t, []byte(source), after, "source bytes for %s", path)
	}
}

func TestRunTimingBudgetFixtures(t *testing.T) {
	for _, assertion := range []string{"assert.Eventually", "require.Eventually", "assert.EventuallyWithT", "require.EventuallyWithT", "assert.Never", "require.Never"} {
		t.Run(assertion+" rejects 50ms", func(t *testing.T) {
			files := map[string]string{"fixture_test.go": fixtureSource(fixtureImports, "TestFixture", assertion+"(t, func() bool { panic(\"must never execute\") }, 50*time.Millisecond, time.Millisecond)")}
			root := t.TempDir()
			writeTimingFixtures(t, root, files)
			assertTimingScan(t, root, files, 1, fixtureDiagnostic("fixture_test.go", 4, assertion, "50ms"))
		})
	}

	for _, tt := range []struct {
		name       string
		expression string
		wantCode   int
		duration   string
	}{
		{"reject 999ms", "999*time.Millisecond", 1, "999ms"},
		{"reject zero", "0", 1, "0s"},
		{"reject negative 50ms", "-50*time.Millisecond", 1, "-50ms"},
		{"reject arithmetic 500ms", "time.Second/2", 1, "500ms"},
		{"reject converted 50ms", "time.Duration(50)*time.Millisecond", 1, "50ms"},
		{"accept exactly 1s", "time.Second", 0, ""},
		{"accept named expression", "short", 0, ""},
		{"accept expression with named term", "short+time.Millisecond", 0, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]string{"fixture_test.go": fixtureSource(fixtureImports, "TestFixture", "const short = 50*time.Millisecond\nassert.Eventually(t, func() bool { panic(\"must never execute\") }, "+tt.expression+", time.Millisecond)")}
			root := t.TempDir()
			writeTimingFixtures(t, root, files)
			wantOutput := ""
			if tt.wantCode == 1 {
				wantOutput = fixtureDiagnostic("fixture_test.go", 5, "assert.Eventually", tt.duration)
			}
			assertTimingScan(t, root, files, tt.wantCode, wantOutput)
		})
	}

	for _, tt := range []struct {
		name    string
		imports string
		body    string
		want    string
	}{
		{"aliased assert and time", `import ("testing"; clock "time"; check "github.com/stretchr/testify/assert")`, `check.EventuallyWithT(t, nil, clock.Duration(50)*clock.Millisecond, 1)`, fixtureDiagnostic("fixture_test.go", 4, "assert.EventuallyWithT", "50ms")},
		{"aliased require", `import ("testing"; clock "time"; check "github.com/stretchr/testify/require")`, `check.Eventually(t, nil, clock.Second/2, 1)`, fixtureDiagnostic("fixture_test.go", 4, "require.Eventually", "500ms")},
		{"unrelated assert import", `import ("testing"; "time"; assert "example.org/assert")`, `assert.Never(t, nil, 50*time.Millisecond, 1)`, ""},
		{"unrelated time import", `import ("testing"; time "example.org/clock"; "github.com/stretchr/testify/assert")`, `assert.Never(t, nil, 50*time.Millisecond, 1)`, ""},
		{"shadowed assert", fixtureImports, `assert := fake(); assert.Never(t, nil, 50000000, 1)`, ""},
		{"shadowed require parameter", fixtureImports, `func(require fake) { require.Eventually(t, nil, 50000000, 1) }(fake{})`, ""},
		{"shadowed time variable", fixtureImports, `time := fake(); assert.Never(t, nil, 50*time.Millisecond, 1)`, ""},
		{"shadowed time conversion", fixtureImports, `time := fake(); assert.Never(t, nil, time.Duration(50000000), 1)`, ""},
		{"shadow ends with block", fixtureImports, "{ assert := fake(); assert.Never(t, nil, 50000000, 1) }\nassert.Never(t, nil, 50*time.Millisecond, 1)", fixtureDiagnostic("fixture_test.go", 5, "assert.Never", "50ms")},
		{"declaration RHS uses import", fixtureImports, `assert := assert.Never(t, nil, 50*time.Millisecond, 1); _ = assert`, fixtureDiagnostic("fixture_test.go", 4, "assert.Never", "50ms")},
		{"dot imports", `import ("testing"; . "time"; . "github.com/stretchr/testify/assert")`, `Never(t, nil, 50*Millisecond, 1)`, ""},
		{"excluded call forms", fixtureImports, "assert.Neverf(t, nil, 50000000, 1, \"message\")\nrequire.Eventuallyf(t, nil, 50000000, 1, \"message\")\nassert.New(t).Never(nil, 50000000, 1)\ntime.Sleep(50*time.Millisecond)", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]string{"fixture_test.go": fixtureSource(tt.imports, "TestFixture", tt.body)}
			root := t.TempDir()
			writeTimingFixtures(t, root, files)
			wantCode := 0
			if tt.want != "" {
				wantCode = 1
			}
			assertTimingScan(t, root, files, wantCode, tt.want)
		})
	}

	for _, path := range []string{"nested/fixture_darwin_test.go", "fixture.go", "vendor/fixture_test.go", "node_modules/fixture_test.go", "testdata/fixture_test.go", ".hidden/fixture_test.go", "_fixtures/fixture_test.go", "nested/testdata/fixture_test.go"} {
		t.Run("file selection "+path, func(t *testing.T) {
			files := map[string]string{path: "//go:build darwin\n\n" + fixtureSource(fixtureImports, "TestFixture", "assert.Never(t, nil, 50*time.Millisecond, 1)")}
			root := t.TempDir()
			writeTimingFixtures(t, root, files)
			wantCode, wantOutput := 0, ""
			if path == "nested/fixture_darwin_test.go" {
				wantCode, wantOutput = 1, fixtureDiagnostic(path, 6, "assert.Never", "50ms")
			}
			assertTimingScan(t, root, files, wantCode, wantOutput)
		})
	}

	t.Run("default root and deterministic diagnostics", func(t *testing.T) {
		root := t.TempDir()
		source := fixtureSource(fixtureImports, "TestFixture", "assert.Never(t, nil, 50000000, 1)")
		files := map[string]string{"z_test.go": source, "a_test.go": source}
		writeTimingFixtures(t, root, files)
		t.Chdir(root)
		var stderr bytes.Buffer
		assert.Equal(t, 1, run(nil, &stderr))
		assert.Equal(t, fixtureDiagnostic("a_test.go", 4, "assert.Never", "50ms")+fixtureDiagnostic("z_test.go", 4, "assert.Never", "50ms"), stderr.String())
		for path, source := range files {
			after, err := os.ReadFile(filepath.Join(root, path))
			require.NoError(t, err)
			assert.Equal(t, []byte(source), after)
		}
	})
}

func TestRunTimingBudgetAllowances(t *testing.T) {
	const path = "vault_external_test.go"
	const function = "TestEmbeddedProcessingWaitsForBackupFreeze"
	t.Run("one exact allowance", func(t *testing.T) {
		root := t.TempDir()
		files := map[string]string{path: fixtureSource(fixtureImports, function, "require.Never(t, nil, 100*time.Millisecond, 10*time.Millisecond)")}
		writeTimingFixtures(t, root, files)
		assertTimingScan(t, root, files, 0, "")
	})

	for _, tt := range []struct {
		name string
		path string
		fn   string
		body string
	}{
		{"extra occurrence", path, function, "require.Never(t, nil, 100*time.Millisecond, 1)\nrequire.Never(t, nil, 100*time.Millisecond, 1)"},
		{"changed duration", path, function, "require.Never(t, nil, 99*time.Millisecond, 1)"},
		{"changed function", path, "TestOther", "require.Never(t, nil, 100*time.Millisecond, 1)"},
		{"changed file", "other_test.go", function, "require.Never(t, nil, 100*time.Millisecond, 1)"},
		{"changed assertion", path, function, "assert.Never(t, nil, 100*time.Millisecond, 1)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			files := map[string]string{tt.path: fixtureSource(fixtureImports, tt.fn, tt.body)}
			writeTimingFixtures(t, root, files)
			line, duration, assertion := 4, "100ms", "require.Never"
			if tt.name == "extra occurrence" {
				line = 5
			}
			if tt.name == "changed duration" {
				duration = "99ms"
			}
			if tt.name == "changed assertion" {
				assertion = "assert.Never"
			}
			assertTimingScan(t, root, files, 1, fixtureDiagnostic(tt.path, line, assertion, duration))
		})
	}
}

func TestRunTimingBudgetErrors(t *testing.T) {
	t.Run("invalid syntax returns 2", func(t *testing.T) {
		root := t.TempDir()
		files := map[string]string{"broken_test.go": "package fixture\nfunc (\n"}
		writeTimingFixtures(t, root, files)
		var stderr bytes.Buffer
		assert.Equal(t, 2, run([]string{root}, &stderr))
		assert.Contains(t, stderr.String(), "check-timing-budgets: parse broken_test.go: broken_test.go:2:")
		assertTimingScan(t, root, files, 2, stderr.String())
	})

	t.Run("missing root returns 2", func(t *testing.T) {
		var stderr bytes.Buffer
		assert.Equal(t, 2, run([]string{filepath.Join(t.TempDir(), "missing")}, &stderr))
		assert.Contains(t, stderr.String(), "check-timing-budgets:")
	})

	t.Run("file root returns 2", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "fixture_test.go")
		require.NoError(t, os.WriteFile(path, []byte("package fixture\n"), 0o644))
		var stderr bytes.Buffer
		assert.Equal(t, 2, run([]string{path}, &stderr))
		assert.Contains(t, stderr.String(), "expected a directory")
	})

	t.Run("excess arguments returns 2", func(t *testing.T) {
		var stderr bytes.Buffer
		assert.Equal(t, 2, run([]string{"one", "two"}, &stderr))
		assert.Equal(t, "usage: check-timing-budgets [directory]\n", stderr.String())
	})
}
