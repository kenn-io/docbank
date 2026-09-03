package providerutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// PythonEnvironment returns the deterministic, ambient-credential-free
// environment used by pinned Python bridge executables.
func PythonEnvironment() []string {
	environment := []string{
		"LANG=C.UTF-8", "LC_ALL=C.UTF-8", "TZ=UTC", "PYTHONHASHSEED=0",
		"PYTHONNOUSERSITE=1", "PYTHONDONTWRITEBYTECODE=1",
	}
	if runtime.GOOS == "windows" {
		if systemRoot := os.Getenv("SystemRoot"); systemRoot != "" {
			environment = append(environment, "SystemRoot="+systemRoot)
		}
	}
	return environment
}

// IsPythonInterpreter reports whether path names a generic Python interpreter
// instead of a pinned bridge executable.
func IsPythonInterpreter(path string) bool {
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(path)), ".exe")
	if name == "py" || name == "pyw" {
		return true
	}
	for _, prefix := range []string{"python", "pypy"} {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(name, prefix)
		if prefix == "python" {
			suffix = strings.TrimPrefix(suffix, "w")
		}
		if suffix == "" {
			return true
		}
		for _, character := range suffix {
			if (character < '0' || character > '9') && character != '.' {
				return false
			}
		}
		return true
	}
	return false
}
