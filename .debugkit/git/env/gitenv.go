// Package gitenv sanitizes inherited environment variables for child git
// processes.
//
// Git gives environment variables such as GIT_DIR, GIT_WORK_TREE, and
// GIT_CONFIG_* precedence over a child command's working directory and local
// repository config. These helpers remove those variables before automation or
// tests initialize and mutate their own repositories.
package gitenv

import (
	"runtime"
	"slices"
	"strings"
)

// StripInherited removes GIT_* variables that can bind a child git process to
// an inherited parent repository, config, identity, or credential prompt.
func StripInherited(env []string) []string {
	return stripInheritedForGOOS(env, runtime.GOOS)
}

// StripAll removes every GIT_* variable and SSH_ASKPASS from env.
//
// This is the safest default for automated git operations and tests: inherited
// variables such as GIT_DIR, GIT_CONFIG_*, GIT_DEFAULT_HASH, or GIT_SSL_* should
// not silently affect throwaway repositories or credential-injected commands.
func StripAll(env []string) []string {
	return stripAllForGOOS(env, runtime.GOOS)
}

func stripAllForGOOS(env []string, goos string) []string {
	return slices.DeleteFunc(cloneEnv(env), func(e string) bool {
		key, _, _ := strings.Cut(e, "=")
		if goos == "windows" {
			key = strings.ToUpper(key)
		}
		return strings.HasPrefix(key, "GIT_") || key == "SSH_ASKPASS"
	})
}

func cloneEnv(env []string) []string {
	if env == nil {
		return []string{}
	}
	return slices.Clone(env)
}

func stripInheritedForGOOS(env []string, goos string) []string {
	return slices.DeleteFunc(cloneEnv(env), func(e string) bool {
		return isInheritedForGOOS(e, goos)
	})
}

func isInheritedForGOOS(e, goos string) bool {
	key, _, _ := strings.Cut(e, "=")
	if goos == "windows" {
		key = strings.ToUpper(key)
	}
	switch key {
	case "GIT_DIR",
		"GIT_WORK_TREE",
		"GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_COMMON_DIR",
		"GIT_NAMESPACE",
		"GIT_PREFIX",
		"GIT_ASKPASS",
		"GIT_SSH_COMMAND",
		"SSH_ASKPASS":
		return true
	}
	if strings.HasPrefix(key, "GIT_CONFIG") ||
		strings.HasPrefix(key, "GIT_AUTHOR_") ||
		strings.HasPrefix(key, "GIT_COMMITTER_") {
		return true
	}
	return false
}
