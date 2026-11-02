package agenthook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/pathresolve"
)

// Hook describes one command registration using Claude Code event and matcher
// names. An empty matcher observes every invocation of the event. A zero timeout
// omits the timeout field and lets the harness use its default.
type Hook struct {
	Event   Event
	Matcher string
	Timeout time.Duration
}

// InstallOptions describes an application's hooks. Set Executable and Arguments
// to have kit construct commands for every supported shell. Command,
// CommandWindows, and CommandPowerShell are raw overrides for the native,
// Win32 argv, and PowerShell forms respectively; do not combine them with
// Executable. Marker must be a stable, application-namespaced substring unique
// to commands the caller owns; it identifies those commands across binary path
// changes. On Windows, Claude Code hooks built from Executable are written in
// exec form, which needs an executable such as an .exe rather than a .cmd or
// .bat shim, and the marker matches the executable and arguments joined by
// spaces. Pi hooks always use exec form on every OS: they require Executable,
// reject raw commands and matchers, and match the marker the same way. Hooks
// defaults to every event supported by the selected profile.
type InstallOptions struct {
	ConfigPath        string
	Executable        string
	Arguments         []string
	Command           string
	CommandWindows    string
	CommandPowerShell string
	Marker            string
	Hooks             []Hook
}

// Result reports the planned or completed config mutation. Data contains the
// complete resulting config and can be used by command-line dump operations.
// Data is nil when an uninstall removes a kit-owned config file, such as Pi's
// extension module once no application's hooks remain.
type Result struct {
	Agent      Agent
	ConfigPath string
	Changed    bool
	Data       []byte
}

// PlanInstall computes an install without writing the config file.
func PlanInstall(agent Agent, opts InstallOptions) (Result, error) {
	opts.Marker = strings.TrimSpace(opts.Marker)
	commands, err := resolveCommands(opts)
	if err != nil {
		return Result{}, err
	}
	spec, ok := profiles[agent]
	if !ok {
		return Result{}, fmt.Errorf("unsupported agent hook integration %q", agent)
	}
	opts.Command, opts.CommandWindows = profileCommands(spec, commands)
	argv, err := execArgv(spec, opts)
	if err != nil {
		return Result{}, err
	}
	if argv != nil {
		// Removal matches the written argv joined by spaces, so validate that.
		opts.Command = strings.Join(argv, " ")
	}
	spec, path, hooks, err := prepareInstall(agent, opts)
	if err != nil {
		return Result{}, err
	}
	data, changed, err := planConfig(
		spec, path, opts.Marker, opts.Command, opts.CommandWindows, argv, hooks, false,
	)
	if err != nil {
		return Result{}, err
	}
	return Result{Agent: agent, ConfigPath: path, Changed: changed, Data: data}, nil
}

// execArgv returns the argv to write in exec form, or nil when the profile
// writes a command string on this platform.
func execArgv(spec profileSpec, opts InstallOptions) ([]string, error) {
	executable := strings.TrimSpace(opts.Executable)
	if spec.format == formatScript && executable == "" {
		return nil, fmt.Errorf(
			"%s hooks need Executable and Arguments; a raw command needs a shell",
			spec.profile.DisplayName,
		)
	}
	if spec.format != formatScript && (executable == "" ||
		spec.windowsCommandStyle != windowsCommandExec || runtime.GOOS != "windows") {
		return nil, nil
	}
	// Node refuses to spawn .cmd and .bat files without a shell:
	// https://nodejs.org/en/blog/vulnerability/april-2024-security-releases-2
	if ext := filepath.Ext(executable); runtime.GOOS == "windows" &&
		(strings.EqualFold(ext, ".cmd") || strings.EqualFold(ext, ".bat")) {
		return nil, fmt.Errorf(
			"%s hooks on Windows cannot run %s shim %s without a shell; "+
				"pass the executable it launches",
			spec.profile.DisplayName, ext, executable,
		)
	}
	return append([]string{executable}, opts.Arguments...), nil
}

// Install merges the application's hooks into the selected agent config.
// Callers must serialize concurrent mutations of the same config path.
func Install(agent Agent, opts InstallOptions) (Result, error) {
	result, err := PlanInstall(agent, opts)
	if err != nil || !result.Changed {
		return result, err
	}
	if err := writeConfig(result.ConfigPath, result.Data); err != nil {
		if errors.Is(err, atomicfile.ErrPublished) {
			return result, err
		}
		return Result{}, err
	}
	return result, nil
}

// PlanUninstall computes removal of every command containing marker without
// writing the config file. A missing config is a successful no-op.
func PlanUninstall(agent Agent, configPath, marker string) (Result, error) {
	spec, ok := profiles[agent]
	if !ok {
		return Result{}, fmt.Errorf("unsupported agent hook integration %q", agent)
	}
	marker = strings.TrimSpace(marker)
	if marker == "" {
		return Result{}, errors.New("agent hook marker is required")
	}
	path, err := resolveConfigPath(agent, configPath)
	if err != nil {
		return Result{}, err
	}
	data, changed, err := planConfig(spec, path, marker, "", "", nil, nil, true)
	if err != nil {
		return Result{}, err
	}
	return Result{Agent: agent, ConfigPath: path, Changed: changed, Data: data}, nil
}

// Uninstall removes every command containing marker from the selected agent
// config while preserving hooks owned by other applications. It deletes a
// kit-owned config file, such as Pi's extension module, once no hooks remain.
// Callers must serialize concurrent mutations of the same config path.
func Uninstall(agent Agent, configPath, marker string) (Result, error) {
	result, err := PlanUninstall(agent, configPath, marker)
	if err != nil || !result.Changed {
		return result, err
	}
	if result.Data == nil {
		if err := os.Remove(result.ConfigPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Result{}, fmt.Errorf("remove agent hook config %s: %w", result.ConfigPath, err)
		}
		return result, nil
	}
	if err := writeConfig(result.ConfigPath, result.Data); err != nil {
		if errors.Is(err, atomicfile.ErrPublished) {
			return result, err
		}
		return Result{}, err
	}
	return result, nil
}

func prepareInstall(agent Agent, opts InstallOptions) (profileSpec, string, []nativeHook, error) {
	spec, ok := profiles[agent]
	if !ok {
		return profileSpec{}, "", nil, fmt.Errorf("unsupported agent hook integration %q", agent)
	}
	command := strings.TrimSpace(opts.Command)
	if command == "" {
		return profileSpec{}, "", nil, errors.New("agent hook command is required")
	}
	marker := strings.TrimSpace(opts.Marker)
	if marker == "" {
		return profileSpec{}, "", nil, errors.New("agent hook marker is required")
	}
	if !strings.Contains(command, marker) {
		return profileSpec{}, "", nil, errors.New("agent hook command must contain its ownership marker")
	}
	path, err := resolveConfigPath(agent, opts.ConfigPath)
	if err != nil {
		return profileSpec{}, "", nil, err
	}

	hooks := opts.Hooks
	if len(hooks) == 0 {
		hooks = make([]Hook, 0, len(spec.profile.SupportedEvents))
		for _, event := range spec.profile.SupportedEvents {
			hooks = append(hooks, Hook{Event: event})
		}
	}
	nativeHooks := make([]nativeHook, 0, len(hooks))
	seen := make(map[string]struct{}, len(hooks))
	for _, hook := range hooks {
		if !slices.Contains(spec.profile.SupportedEvents, hook.Event) {
			return profileSpec{}, "", nil, fmt.Errorf("%s does not support %s hooks", spec.profile.DisplayName, hook.Event)
		}
		if hook.Timeout < 0 || hook.Timeout%spec.timeoutUnit != 0 {
			return profileSpec{}, "", nil, fmt.Errorf(
				"%s hook timeout must be a non-negative multiple of %s",
				hook.Event, spec.timeoutUnit,
			)
		}
		timeout := int(hook.Timeout / spec.timeoutUnit)
		if spec.format == formatHermesYAML && hook.Timeout > 300*time.Second {
			return profileSpec{}, "", nil, errors.New("Hermes hook timeout must not exceed 300 seconds")
		}
		// The generated module runs every command for its event, so a matcher would be dropped silently.
		if spec.format == formatScript && strings.TrimSpace(hook.Matcher) != "" {
			return profileSpec{}, "", nil, fmt.Errorf("%s hooks do not support matchers", spec.profile.DisplayName)
		}
		matcher := nativeMatcher(spec, strings.TrimSpace(hook.Matcher))
		if spec.format == formatHermesYAML && matcher != "" &&
			hook.Event != EventPreToolUse && hook.Event != EventPostToolUse {
			return profileSpec{}, "", nil, errors.New("Hermes only supports matchers on PreToolUse and PostToolUse hooks")
		}
		key := string(hook.Event) + "\x00" + matcher
		if _, exists := seen[key]; exists {
			return profileSpec{}, "", nil, fmt.Errorf("duplicate %s hook matcher %q", hook.Event, hook.Matcher)
		}
		seen[key] = struct{}{}
		nativeHooks = append(nativeHooks, nativeHook{
			event: hook.Event, name: nativeEventName(spec, hook.Event),
			matcher: matcher, timeout: timeout,
		})
	}
	return spec, path, nativeHooks, nil
}

type nativeHook struct {
	event   Event
	name    string
	matcher string
	timeout int
}

func resolveConfigPath(agent Agent, override string) (string, error) {
	if path := strings.TrimSpace(override); path != "" {
		return path, nil
	}
	return ConfigPath(agent)
}

func planConfig(
	spec profileSpec,
	path, marker, command, commandWindows string,
	argv []string,
	hooks []nativeHook,
	uninstall bool,
) ([]byte, bool, error) {
	switch spec.format {
	case formatNestedJSON:
		return planNestedJSONConfig(spec, path, marker, command, commandWindows, argv, hooks, uninstall)
	case formatDirectJSON:
		return planDirectJSONConfig(
			spec, path, marker, command, commandWindows, hooks, uninstall,
		)
	case formatHermesYAML:
		return planHermesConfig(path, marker, command, hooks, uninstall)
	case formatScript:
		return planScriptConfig(spec, path, marker, argv, hooks, uninstall)
	default:
		return nil, false, errors.New("unsupported agent hook config format")
	}
}

func writeConfig(path string, data []byte) error {
	writePath := path
	opts := []atomicfile.Option{atomicfile.WithPerm(0o600), atomicfile.WithPreserveMode()}
	info, err := os.Lstat(path)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		// Write through the original path so atomicfile resolves the link
		// chain again at commit and refuses if it was retargeted. Follow
		// links only here: a path that was not a link keeps atomicfile's
		// default, which refuses a link swapped in before the write.
		opts = append(opts, atomicfile.WithFollowLink())
		writePath, err = pathresolve.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("resolve agent hook config symlink %s: %w", path, err)
		}
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("inspect agent hook config %s: %w", path, err)
	}
	dir := filepath.Dir(writePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create agent hook config directory %s: %w", dir, err)
	}
	if err := writeAtomicFile(path, data, opts...); err != nil {
		return fmt.Errorf("replace agent hook config %s: %w", path, err)
	}
	return nil
}

var writeAtomicFile = atomicfile.WriteFile
