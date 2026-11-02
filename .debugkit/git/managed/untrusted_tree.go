package managedworktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
	"go.kenn.io/kit/git/internal/shellquote"
	"go.kenn.io/kit/pathresolve"
)

// untrustedTreeIsolation neutralizes Git programs that a fetched tree can
// select through tracked hooks or attributes. The repository and its
// configuration remain trusted; only the fetched commit is treated as
// untrusted.
type untrustedTreeIsolation struct {
	runner             gitcmd.Runner
	config             []gitcmd.Config
	mergeDriverCommand string
}

// Git invokes these through its compiled-in shell because they contain shell
// syntax. The diff and textconv replacements use only shell builtins. The
// merge replacement invokes the resolved Git executable by absolute path, so
// an untrusted worktree cannot steer a helper through PATH. The diff replacement
// emits a simple old/new rendering; the merge replacement performs a fixed-label
// diff3 merge.
const (
	safeExternalDiffCommand = `f() { emit() { prefix=$1; file=$2; line=; while IFS= read -r line; do printf '%s%s\n' "$prefix" "$line"; line=; done < "$file"; case "$line" in "") ;; *) printf '%s%s\n' "$prefix" "$line"; printf '%s\n' '\ No newline at end of file' ;; esac; }; emit - "$2"; emit + "$5"; }; f`
	safeTextconvCommand     = `f() { line=; while IFS= read -r line; do printf '%s\n' "$line"; line=; done < "$1"; case "$line" in "") ;; *) printf '%s' "$line" ;; esac; }; f`
)

func resolveMergeDriverGitPath() (string, error) {
	path, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("resolve Git executable for safe merge driver: %w", err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("make Git executable path absolute: %w", err)
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = filepath.ToSlash(path)
	}
	return path, nil
}

func safeMergeDriverCommand(gitPath, classifierDir string) string {
	// Git expands merge-driver placeholders before the shell parses the
	// command. Double literal percent signs before applying shell quoting.
	git := shellquote.Single(strings.ReplaceAll(gitPath, "%", "%%"))
	classifier := shellquote.Single(strings.ReplaceAll(classifierDir, "%", "%%"))
	ceiling := filepath.Dir(classifierDir)
	if runtime.GOOS == "windows" {
		classifier = shellquote.Single(strings.ReplaceAll(
			filepath.ToSlash(classifierDir), "%", "%%",
		))
		ceiling = filepath.ToSlash(ceiling)
	}
	ceiling = shellquote.Single(strings.ReplaceAll(ceiling, "%", "%%"))
	return `f() { [ -x ` + git + ` ] || return 129; ` +
		`case "$2" in /*|[A-Za-z]:/*) current=$2 ;; *) current=$PWD/$2 ;; esac; ` +
		`case "$3" in /*|[A-Za-z]:/*) base=$3 ;; *) base=$PWD/$3 ;; esac; ` +
		`case "$4" in /*|[A-Za-z]:/*) other=$4 ;; *) other=$PWD/$4 ;; esac; ` +
		`unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY ` +
		`GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR GIT_NAMESPACE GIT_PREFIX || return 129; ` +
		`classify() { output=$(GIT_CONFIG_COUNT=0 GIT_ATTR_NOSYSTEM=1 GIT_CEILING_DIRECTORIES=` +
		ceiling + ` ` + git +
		` -c core.attributesFile= -c core.bigFileThreshold=1023m -C ` + classifier +
		` diff --no-index --numstat --no-ext-diff --no-textconv -- "$1" "$2"); ` +
		`status=$?; [ "$status" -le 1 ] || return 129; ` +
		`case "$output" in -*) return 1 ;; "") [ "$status" -eq 0 ] || return 129 ;; ` +
		`[0-9]*) ;; *) return 129 ;; esac; return 0; }; ` +
		`classify "$current" "$base"; status=$?; ` +
		`[ "$status" -eq 1 ] && return 1; [ "$status" -eq 0 ] || return "$status"; ` +
		`classify "$base" "$other"; status=$?; ` +
		`[ "$status" -eq 1 ] && return 1; [ "$status" -eq 0 ] || return "$status"; ` +
		git + ` merge-file --diff3 --marker-size="$1" -L current -L base -L other "$2" "$3" "$4"; ` +
		`return $?; }; f %L "%A" "%O" "%B"`
}

var untrustedTreeGitVersionPattern = regexp.MustCompile(
	`(?i)git version (\d+)\.(\d+)(?:\.(\d+))?(?:\.windows\.(\d+))?(?:\s|$)`,
)

func validateUntrustedTreeCheckoutGitVersion(
	ctx context.Context, root string,
) error {
	out, err := runLifecycleGit(ctx, root, "version")
	if err != nil {
		return &ChangeRequestError{
			Kind:    ChangeRequestUnsupportedGit,
			Message: "determine Git version before untrusted-tree checkout: " + strings.TrimSpace(string(out)),
			Cause:   err,
		}
	}
	if supportsUntrustedTreeCheckoutGitVersion(string(out), runtime.GOOS) {
		return nil
	}
	requirement := "Git 2.42.0 or newer"
	if runtime.GOOS == "windows" || isGitForWindowsVersion(string(out)) {
		requirement = "Git for Windows 2.53.0.windows.3 or newer"
	}
	return &ChangeRequestError{
		Kind:    ChangeRequestUnsupportedGit,
		Message: "untrusted-tree checkout requires " + requirement,
	}
}

func supportsUntrustedTreeCheckoutGitVersion(output, goos string) bool {
	match := untrustedTreeGitVersionPattern.FindStringSubmatch(
		strings.TrimSpace(output),
	)
	if len(match) == 0 {
		return false
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	patch, _ := strconv.Atoi(match[3])
	if major < 2 || major == 2 && minor < 42 {
		return false
	}
	if goos != "windows" && match[4] == "" {
		return true
	}
	if major != 2 || minor != 53 || patch != 0 {
		return major > 2 ||
			major == 2 && (minor > 53 || minor == 53 && patch > 0)
	}
	windowsPatch, err := strconv.Atoi(match[4])
	return err == nil && windowsPatch >= 3
}

func isGitForWindowsVersion(output string) bool {
	match := untrustedTreeGitVersionPattern.FindStringSubmatch(
		strings.TrimSpace(output),
	)
	return len(match) != 0 && match[4] != ""
}

func validateWorktreeConfigCompatibility(
	ctx context.Context, root string,
) error {
	if value, present, err := localGitConfigValue(
		ctx, root, "core.worktree",
	); err != nil {
		return err
	} else if present {
		return fmt.Errorf(
			"cannot enable worktree-scoped configuration while shared core.worktree is set to %q",
			value,
		)
	}
	if value, present, err := localGitConfigValue(
		ctx, root, "core.bare",
	); err != nil {
		return err
	} else if present && strings.EqualFold(value, "true") {
		return errors.New("cannot enable worktree-scoped configuration while shared core.bare=true")
	}
	return nil
}

func localGitConfigValue(
	ctx context.Context, root, key string,
) (string, bool, error) {
	args := []string{"config", "--local", "--get", key}
	if key == "core.bare" {
		args = []string{"config", "--local", "--type=bool", "--get", key}
	}
	out, err := runLifecycleGit(ctx, root, args...)
	if err == nil {
		return strings.TrimSpace(string(out)), true, nil
	}
	if gitcmd.IsExitCode(err, 1) {
		return "", false, nil
	}
	return "", false, fmt.Errorf(
		"inspect shared %s configuration: %w: %s",
		key, err, strings.TrimSpace(string(out)),
	)
}

func prepareUntrustedTreeIsolation(
	ctx context.Context, root string,
) (untrustedTreeIsolation, error) {
	if err := rejectCommandScopeIsolationOverrides(
		ctx, root, lifecycleRunner(ctx),
	); err != nil {
		return untrustedTreeIsolation{}, err
	}
	gitPath, err := resolveMergeDriverGitPath()
	if err != nil {
		return untrustedTreeIsolation{}, err
	}
	hooksPath, err := managedEmptyHooksPath(ctx, root)
	if err != nil {
		return untrustedTreeIsolation{}, err
	}

	config := []gitcmd.Config{
		{Key: "core.hooksPath", Value: hooksPath},
		{Key: "core.fsmonitor", Value: "false"},
		{Key: "submodule.recurse", Value: "false"},
		{Key: "fetch.recurseSubmodules", Value: "false"},
	}
	runner := lifecycleRunner(ctx)
	for _, entry := range config {
		runner = runner.WithConfig(entry.Key, entry.Value)
	}
	keys, err := isolationConfigKeys(ctx, root, runner)
	if err != nil {
		return untrustedTreeIsolation{}, err
	}
	for _, name := range configuredGitHooks(keys) {
		entry := gitcmd.Config{Key: "hook." + name + ".enabled", Value: "false"}
		config = append(config, entry)
		runner = runner.WithConfig(entry.Key, entry.Value)
	}
	return untrustedTreeIsolation{runner: runner, config: config, mergeDriverCommand: safeMergeDriverCommand(gitPath, hooksPath)}, nil
}

func rejectCommandScopeIsolationOverrides(
	ctx context.Context, root string, runner gitcmd.Runner,
) error {
	runner.Env = withoutGitRepositoryBindings(runner.Env)
	runner.StripEnv = false
	runner.NullGlobalConfig = false
	runner.NoSystemConfig = false
	out, err := runLifecycleGitWithRunner(
		ctx, runner, root,
		"config", "--null", "--show-scope", "--show-origin", "--name-only",
		"--includes", "--list",
	)
	if err != nil {
		return fmt.Errorf(
			"inspect command-scope Git configuration: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	fields := bytes.Split(out, []byte{0})
	var overrides []string
	for index := 0; index+2 < len(fields); index += 3 {
		if string(fields[index]) != "command" {
			continue
		}
		key := strings.TrimSpace(string(fields[index+2]))
		if isolationSensitiveConfigKey(key) {
			overrides = append(overrides, key)
		}
	}
	if len(overrides) == 0 {
		return nil
	}
	sort.Strings(overrides)
	return fmt.Errorf(
		"command-scope Git configuration cannot be isolated: %s",
		strings.Join(overrides, ", "),
	)
}

func isolationSensitiveConfigKey(key string) bool {
	lower := strings.ToLower(key)
	switch lower {
	case "core.hookspath", "core.fsmonitor", "core.worktree",
		"submodule.recurse", "fetch.recursesubmodules":
		return true
	}
	if strings.HasPrefix(lower, "hook.") &&
		(strings.HasSuffix(lower, ".command") ||
			strings.HasSuffix(lower, ".event") ||
			strings.HasSuffix(lower, ".enabled")) {
		return true
	}
	if strings.HasPrefix(lower, "submodule.") &&
		strings.HasSuffix(lower, ".fetchrecursesubmodules") {
		return true
	}
	return configuredAttributeDrivers([]string{key}).configured()
}

func managedEmptyHooksPath(ctx context.Context, root string) (string, error) {
	out, err := runLifecycleGitWithRunner(
		ctx, lifecycleRunner(ctx).WithConfig("core.hooksPath", os.DevNull),
		root, "rev-parse", "--git-common-dir",
	)
	if err != nil {
		return "", fmt.Errorf(
			"resolve common Git directory: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	commonDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(root, commonDir)
	}
	hooksPath := filepath.Join(commonDir, "kit-managed", "empty-hooks")
	if err := os.MkdirAll(hooksPath, 0o700); err != nil {
		return "", fmt.Errorf("create managed empty hooks directory: %w", err)
	}
	if err := os.Chmod(hooksPath, 0o700); err != nil {
		return "", fmt.Errorf("secure managed empty hooks directory: %w", err)
	}
	entries, err := os.ReadDir(hooksPath)
	if err != nil {
		return "", fmt.Errorf("inspect managed empty hooks directory: %w", err)
	}
	if len(entries) != 0 {
		return "", fmt.Errorf(
			"managed empty hooks directory is not empty: %s", hooksPath,
		)
	}
	return hooksPath, nil
}

func completeUntrustedTreeIsolation(
	ctx context.Context, worktreePath string, isolation untrustedTreeIsolation,
) (untrustedTreeIsolation, error) {
	keys, err := isolationConfigKeys(ctx, worktreePath, isolation.runner)
	if err != nil {
		return untrustedTreeIsolation{}, err
	}
	drivers := neutralizeAttributeDrivers(
		configuredAttributeDrivers(keys), isolation.mergeDriverCommand,
	)
	for _, name := range configuredGitHooks(keys) {
		drivers = append(drivers, gitcmd.Config{Key: "hook." + name + ".enabled", Value: "false"})
	}
	submodules, err := submoduleFetchRecurseConfig(
		ctx, worktreePath, isolation.runner,
	)
	if err != nil {
		return untrustedTreeIsolation{}, err
	}
	drivers = append(drivers, submodules...)
	existing := make(map[string]struct{}, len(isolation.config))
	for _, entry := range isolation.config {
		existing[entry.Key] = struct{}{}
	}
	for _, entry := range drivers {
		if _, ok := existing[entry.Key]; ok {
			continue
		}
		existing[entry.Key] = struct{}{}
		isolation.config = append(isolation.config, entry)
		isolation.runner = isolation.runner.WithConfig(entry.Key, entry.Value)
	}
	return isolation, nil
}

func isolationConfigKeys(ctx context.Context, path string, runner gitcmd.Runner) ([]string, error) {
	checkoutKeys, err := gitConfigKeys(ctx, path, runner)
	if err != nil {
		return nil, err
	}
	ambientKeys, err := ambientGitConfigKeys(ctx, path, runner)
	if err != nil {
		return nil, err
	}
	return slices.Concat(checkoutKeys, ambientKeys), nil
}

func configuredGitHooks(keys []string) []string {
	hooks := make(map[string]struct{})
	for _, key := range keys {
		if name, ok := configuredSubsectionName(
			key, "hook", []string{".command", ".event"},
		); ok {
			hooks[name] = struct{}{}
		}
	}
	names := make([]string, 0, len(hooks))
	for name := range hooks {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func submoduleFetchRecurseConfig(
	ctx context.Context, worktreePath string, runner gitcmd.Runner,
) ([]gitcmd.Config, error) {
	treeEntry, err := runLifecycleGitWithRunner(
		ctx, runner, worktreePath,
		"ls-tree", "--name-only", "-z", "HEAD", "--", ".gitmodules",
	)
	if err != nil {
		return nil, fmt.Errorf(
			"inspect merge request tree for submodules: %w: %s",
			err, strings.TrimSpace(string(treeEntry)),
		)
	}
	if len(treeEntry) == 0 {
		return nil, nil
	}
	out, err := runLifecycleGitWithRunner(
		ctx, runner, worktreePath,
		"config", "--null", "--name-only", "--blob", "HEAD:.gitmodules",
		"--list",
	)
	if err != nil {
		if gitcmd.IsExitCode(err, 1) {
			return nil, nil
		}
		return nil, fmt.Errorf(
			"inspect committed submodule recursion configuration: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	fields := bytes.Split(out, []byte{0})
	config := make([]gitcmd.Config, 0, len(fields))
	for _, field := range fields {
		if name, ok := configuredSubmoduleName(string(field)); ok {
			config = append(config, gitcmd.Config{
				Key:   "submodule." + name + ".fetchRecurseSubmodules",
				Value: "false",
			})
		}
	}
	return config, nil
}

func configuredSubmoduleName(key string) (string, bool) {
	return configuredSubsectionName(
		key, "submodule", []string{".path", ".fetchrecursesubmodules"},
	)
}

func configuredSubsectionName(
	key, section string, suffixes []string,
) (string, bool) {
	lower := strings.ToLower(key)
	prefix := strings.ToLower(section) + "."
	if !strings.HasPrefix(lower, prefix) {
		return "", false
	}
	start := len(prefix)
	for _, suffix := range suffixes {
		if !strings.HasSuffix(lower, suffix) {
			continue
		}
		end := len(key) - len(suffix)
		if end <= start {
			return "", false
		}
		return key[start:end], true
	}
	return "", false
}

func gitConfigKeys(
	ctx context.Context, worktreePath string, runner gitcmd.Runner,
) ([]string, error) {
	out, err := runLifecycleGitWithRunner(
		ctx, runner, worktreePath,
		"config", "--null", "--name-only", "--includes", "--list",
	)
	if err != nil {
		return nil, fmt.Errorf(
			"inspect effective Git configuration: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	fields := bytes.Split(out, []byte{0})
	keys := make([]string, 0, len(fields))
	for _, field := range fields {
		if key := strings.TrimSpace(string(field)); key != "" {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func ambientGitConfigKeys(
	ctx context.Context, worktreePath string, runner gitcmd.Runner,
) ([]string, error) {
	// The first scan exactly matches checkout. This second scan preserves
	// explicit global/system and command-scope selectors so persistent
	// isolation also covers ordinary Git commands run outside the
	// application's stripped environment.
	runner.Env = withoutGitRepositoryBindings(runner.Env)
	runner.StripEnv = false
	runner.NullGlobalConfig = false
	runner.NoSystemConfig = false
	return gitConfigKeys(ctx, worktreePath, runner)
}

func rejectConfigSourcesInsideWorktree(
	ctx context.Context, dir, worktreePath string, runner gitcmd.Runner,
) error {
	runner.Env = withoutGitRepositoryBindings(runner.Env)
	runner.StripEnv = false
	runner.NullGlobalConfig = false
	runner.NoSystemConfig = false
	worktree, err := pathresolve.EvalSymlinksAllowMissing(worktreePath)
	if err != nil {
		return fmt.Errorf("resolve worktree configuration boundary: %w", err)
	}
	worktree = lexicalWorktreePath(worktree)
	lexicalWorktree := lexicalWorktreePath(worktreePath)
	checkPath := func(configPath string) error {
		if !filepath.IsAbs(configPath) {
			configPath = worktreePath + string(filepath.Separator) + configPath
		}
		resolved, err := pathresolve.EvalSymlinksAllowMissing(configPath)
		if err != nil {
			return fmt.Errorf("resolve Git configuration path: %w", err)
		}
		if pathWithinRoot(lexicalWorktree, lexicalWorktreePath(configPath)) ||
			pathWithinRoot(worktree, lexicalWorktreePath(resolved)) ||
			pathWithinRootByIdentity(worktreePath, configPath) ||
			pathWithinRootByIdentity(worktreePath, resolved) {
			return fmt.Errorf("Git configuration inside merge request worktree is not allowed: %s", configPath)
		}
		return nil
	}
	envValue := func(name string) (string, bool) {
		for _, entry := range slices.Backward(runner.Env) {
			key, value, _ := strings.Cut(entry, "=")
			if key == name || (runtime.GOOS == "windows" && strings.EqualFold(key, name)) {
				return value, true
			}
		}
		return "", false
	}
	// Unlike config origins, Git's selected paths include files that do not yet exist.
	for _, selector := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"} {
		out, err := runLifecycleGitWithRunner(ctx, runner, dir, "var", selector)
		if gitcmd.IsExitCode(err, 1) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect %s: %w", selector, err)
		}
		paths := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
		// git var cleans parent components, but Git opens configuration paths
		// as written. Preserve both explicit selectors and raw default paths.
		if value, explicit := envValue(selector); explicit {
			paths = []string{value}
		} else if selector == "GIT_CONFIG_GLOBAL" {
			home, hasHome := envValue("HOME")
			if hasHome {
				paths = append(paths, home+"/.gitconfig")
			}
			xdg, _ := envValue("XDG_CONFIG_HOME")
			if xdg != "" {
				paths = append(paths, xdg+"/git/config")
			} else if hasHome {
				paths = append(paths, home+"/.config/git/config")
			}
		}
		for _, path := range paths {
			if err := checkPath(path); err != nil {
				return err
			}
		}
	}
	out, err := runLifecycleGitWithRunner(
		ctx, runner, dir,
		"config", "--null", "--show-origin", "--name-only",
		"--includes", "--list",
	)
	if err != nil {
		return fmt.Errorf(
			"inspect Git configuration origins: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	fields := bytes.Split(out, []byte{0})
	for index := 0; index+1 < len(fields); index += 2 {
		origin := string(fields[index])
		if !strings.HasPrefix(origin, "file:") {
			continue
		}
		configPath := strings.TrimPrefix(origin, "file:")
		if !filepath.IsAbs(configPath) {
			configPath = dir + string(filepath.Separator) + configPath
		}
		if err := checkPath(configPath); err != nil {
			return err
		}
	}
	out, err = runLifecycleGitWithRunner(ctx, runner, dir,
		"config", "--null", "--show-origin", "--includes", "--path", "--get-regexp", `^(include\.path|includeif\..*\.path)$`)
	if gitcmd.IsExitCode(err, 1) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Git configuration includes: %w", err)
	}
	fields = bytes.Split(out, []byte{0})
	for index := 0; index+1 < len(fields); index += 2 {
		_, configPath, _ := strings.Cut(string(fields[index+1]), "\n")
		if !filepath.IsAbs(configPath) {
			origin, file := strings.CutPrefix(string(fields[index]), "file:")
			if !file {
				return errors.New("relative config includes must come from files")
			}
			if !filepath.IsAbs(origin) {
				origin = dir + string(filepath.Separator) + origin
			}
			originDir, _ := filepath.Split(origin)
			configPath = originDir + configPath
		}
		if err := checkPath(configPath); err != nil {
			return err
		}
	}
	return nil
}

func pathWithinRootByIdentity(root, path string) bool {
	rootInfo, err := os.Stat(root)
	if err != nil {
		return false
	}
	for current := path; ; current = filepath.Dir(current) {
		if info, statErr := os.Stat(current); statErr == nil &&
			os.SameFile(rootInfo, info) {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
	}
}

func lexicalWorktreePath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = path
	}
	absolute = filepath.Clean(absolute)
	if filepath.Separator == '\\' {
		absolute = strings.ToLower(absolute)
	}
	return absolute
}

func withoutGitRepositoryBindings(env []string) []string {
	clean := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "GIT_DIR",
			"GIT_WORK_TREE",
			"GIT_INDEX_FILE",
			"GIT_OBJECT_DIRECTORY",
			"GIT_ALTERNATE_OBJECT_DIRECTORIES",
			"GIT_COMMON_DIR",
			"GIT_NAMESPACE",
			"GIT_PREFIX":
			continue
		}
		clean = append(clean, entry)
	}
	return clean
}

type attributeDrivers struct {
	filters map[string]struct{}
	diffs   map[string]struct{}
	merges  map[string]struct{}
}

func configuredAttributeDrivers(keys []string) attributeDrivers {
	drivers := attributeDrivers{
		filters: make(map[string]struct{}),
		diffs:   make(map[string]struct{}),
		merges:  make(map[string]struct{}),
	}
	for _, key := range keys {
		lower := strings.ToLower(key)
		switch {
		case strings.HasPrefix(lower, "filter."):
			if driver, ok := configuredDriverName(
				key, []string{".clean", ".smudge", ".process", ".required"},
			); ok {
				drivers.filters[driver] = struct{}{}
			}
		case strings.HasPrefix(lower, "diff."):
			if driver, ok := configuredDriverName(
				key, []string{".command", ".textconv"},
			); ok {
				drivers.diffs[driver] = struct{}{}
			}
		case strings.HasPrefix(lower, "merge."):
			if driver, ok := configuredDriverName(
				key, []string{".driver"},
			); ok {
				drivers.merges[driver] = struct{}{}
			}
		}
	}
	return drivers
}

func (drivers attributeDrivers) configured() bool {
	return len(drivers.filters)+len(drivers.diffs)+len(drivers.merges) != 0
}

func neutralizeAttributeDrivers(
	drivers attributeDrivers, mergeDriverCommand string,
) []gitcmd.Config {
	var config []gitcmd.Config
	for _, driver := range sortedDriverNames(drivers.filters) {
		prefix := "filter." + driver
		config = append(config,
			gitcmd.Config{Key: prefix + ".clean", Value: ""},
			gitcmd.Config{Key: prefix + ".smudge", Value: ""},
			gitcmd.Config{Key: prefix + ".process", Value: ""},
			gitcmd.Config{Key: prefix + ".required", Value: "false"},
		)
	}
	for _, driver := range sortedDriverNames(drivers.diffs) {
		prefix := "diff." + driver
		config = append(config,
			gitcmd.Config{Key: prefix + ".command", Value: safeExternalDiffCommand},
			gitcmd.Config{Key: prefix + ".textconv", Value: safeTextconvCommand},
		)
	}
	for _, driver := range sortedDriverNames(drivers.merges) {
		config = append(config, gitcmd.Config{
			Key: "merge." + driver + ".driver", Value: mergeDriverCommand,
		})
	}
	return config
}

func configuredDriverName(key string, suffixes []string) (string, bool) {
	lower := strings.ToLower(key)
	start := strings.IndexByte(key, '.') + 1
	if start == 0 {
		return "", false
	}
	for _, suffix := range suffixes {
		if strings.HasSuffix(lower, suffix) {
			end := len(key) - len(suffix)
			if end <= start {
				return "", false
			}
			return key[start:end], true
		}
	}
	return "", false
}

func sortedDriverNames(drivers map[string]struct{}) []string {
	names := make([]string, 0, len(drivers))
	for name := range drivers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func persistUntrustedTreeIsolation(
	ctx context.Context, root, worktreePath string,
	isolation untrustedTreeIsolation,
) error {
	if out, err := runLifecycleGitWithRunner(
		ctx, isolation.runner, root,
		"config", "extensions.worktreeConfig", "true",
	); err != nil {
		return fmt.Errorf(
			"enable worktree configuration: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	for _, entry := range isolation.config {
		if out, err := runLifecycleGitWithRunner(
			ctx, isolation.runner, worktreePath,
			"config", "--worktree", entry.Key, entry.Value,
		); err != nil {
			return fmt.Errorf(
				"persist untrusted-tree isolation for %s: %w: %s",
				entry.Key, err, strings.TrimSpace(string(out)),
			)
		}
	}
	return nil
}

func materializeUntrustedTree(
	ctx context.Context, worktreePath string,
	isolation untrustedTreeIsolation,
) error {
	worktreePath, err := filepath.Abs(worktreePath)
	if err != nil {
		return fmt.Errorf("resolve merge request worktree path: %w", err)
	}
	runner := isolation.runner
	runner.Env = withoutGitRepositoryBindings(runner.Env)
	out, err := runLifecycleGitWithRunner(
		ctx, runner, worktreePath,
		"--work-tree="+worktreePath, "reset", "--hard", "HEAD",
	)
	if err != nil {
		return fmt.Errorf(
			"materialize merge request worktree: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	return nil
}
