package managedworktree

import (
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	gitenv "go.kenn.io/kit/git/env"
)

func TestMain(m *testing.M) {
	if os.Getenv("KIT_TEST_HOOK_EXIT") == "1" {
		os.Exit(m.Run())
	}
	os.Exit(runManagedTests(m))
}

func runManagedTests(m *testing.M) int {
	flag.Parse()
	parallelExplicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "test.parallel" {
			parallelExplicit = true
		}
	})
	parallel, err := strconv.Atoi(flag.Lookup("test.parallel").Value.String())
	if err != nil {
		panic(err)
	}
	parallelLimit := 4
	if runtime.GOOS == "windows" {
		parallelLimit = 2
	}
	// Windows Git subprocesses need headroom on the 4-vCPU CI runner.
	if !parallelExplicit && parallel > parallelLimit {
		if err := flag.Set("test.parallel", strconv.Itoa(parallelLimit)); err != nil {
			panic(err)
		}
	}
	home, err := os.MkdirTemp("", "kit-managed-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(home)
	globalConfig := filepath.Join(home, "global.gitconfig")
	if err := os.WriteFile(globalConfig, nil, 0o600); err != nil {
		panic(err)
	}
	environment := os.Environ()
	cleanEnvironment := gitenv.StripAll(environment)
	for _, entry := range environment {
		if slices.Contains(cleanEnvironment, entry) {
			continue
		}
		key, _, _ := strings.Cut(entry, "=")
		if err := os.Unsetenv(key); err != nil {
			panic(err)
		}
	}
	for key, value := range map[string]string{
		"HOME": home, "USERPROFILE": home, "XDG_CONFIG_HOME": home,
		"GIT_CONFIG_GLOBAL": globalConfig, "GIT_CONFIG_NOSYSTEM": "1",
	} {
		if err := os.Setenv(key, value); err != nil {
			panic(err)
		}
	}
	return m.Run()
}
