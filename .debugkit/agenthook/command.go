package agenthook

import (
	"errors"
	"runtime"
	"strings"
)

// Commands contains hook command lines for the current platform, POSIX shells,
// the Win32 argv convention, and PowerShell. Profiles select the command syntax
// their config fields require.
type Commands struct {
	Native     string
	POSIX      string
	Windows    string
	PowerShell string
}

// BuildCommand quotes an executable and arguments for agent hook config. It
// returns an error when executable is blank.
func BuildCommand(executable string, arguments ...string) (Commands, error) {
	executable = strings.TrimSpace(executable)
	if executable == "" {
		return Commands{}, errors.New("agent hook executable is required")
	}
	argv := append([]string{executable}, arguments...)
	posix := make([]string, 0, len(argv))
	windows := make([]string, 0, len(argv))
	powerShell := make([]string, 0, len(argv))
	for _, arg := range argv {
		posix = append(posix, quotePOSIXArgument(arg))
		windows = append(windows, quoteWindowsArgument(arg))
		powerShell = append(powerShell, quotePowerShellArgument(arg))
	}
	commands := Commands{
		Native:     strings.Join(posix, " "),
		POSIX:      strings.Join(posix, " "),
		Windows:    strings.Join(windows, " "),
		PowerShell: "& " + strings.Join(powerShell, " "),
	}
	if runtime.GOOS == "windows" {
		commands.Native = commands.Windows
	}
	return commands, nil
}

func resolveCommands(opts InstallOptions) (Commands, error) {
	hasExecutable := strings.TrimSpace(opts.Executable) != ""
	hasOverride := strings.TrimSpace(opts.Command) != "" ||
		strings.TrimSpace(opts.CommandWindows) != "" ||
		strings.TrimSpace(opts.CommandPowerShell) != ""
	if hasExecutable && hasOverride {
		return Commands{}, errors.New("agent hook executable and command override are mutually exclusive")
	}
	if hasExecutable {
		return BuildCommand(opts.Executable, opts.Arguments...)
	}
	if len(opts.Arguments) > 0 {
		return Commands{}, errors.New("agent hook arguments require an executable")
	}
	return Commands{
		Native: opts.Command, POSIX: opts.Command, Windows: opts.CommandWindows,
		PowerShell: opts.CommandPowerShell,
	}, nil
}

func profileCommands(spec profileSpec, commands Commands) (native, windows string) {
	native = commands.Native
	switch spec.windowsCommandStyle {
	case windowsCommandNested:
		windows = commands.Windows
	case windowsCommandPowerShell:
		native = commands.POSIX
		windows = commands.PowerShell
	default:
	}
	return native, windows
}

func quotePOSIXArgument(arg string) string {
	if arg != "" && strings.IndexFunc(arg, unsafePOSIXArgumentRune) == -1 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
}

func unsafePOSIXArgumentRune(r rune) bool {
	return r != '/' && r != '.' && r != '-' && r != '_' && r != '+' && r != ':' &&
		(r < '0' || r > '9') &&
		(r < 'A' || r > 'Z') &&
		(r < 'a' || r > 'z')
}

// quoteWindowsArgument implements the CommandLineToArgvW quoting convention.
func quoteWindowsArgument(arg string) string {
	if arg != "" && !strings.ContainsAny(arg, " \t\n\v\f\r\"") {
		return arg
	}
	var quoted strings.Builder
	quoted.WriteByte('"')
	backslashes := 0
	for _, r := range arg {
		if r == '\\' {
			backslashes++
			continue
		}
		if r == '"' {
			quoted.WriteString(strings.Repeat("\\", backslashes*2+1))
			quoted.WriteRune(r)
			backslashes = 0
			continue
		}
		quoted.WriteString(strings.Repeat("\\", backslashes))
		backslashes = 0
		quoted.WriteRune(r)
	}
	quoted.WriteString(strings.Repeat("\\", backslashes*2))
	quoted.WriteByte('"')
	return quoted.String()
}

func quotePowerShellArgument(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", "''") + "'"
}
