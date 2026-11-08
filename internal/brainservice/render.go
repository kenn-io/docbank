// Package brainservice renders and explicitly manages per-user publisher jobs.
// Callers supply the current user's identity and absolute locations; Render
// performs no I/O. Only Install and Uninstall change a launcher.
//
// Task Scheduler expands environment references in Exec actions. Windows paths
// containing paired percent signs are rejected rather than redirected through
// startup environment changes. A single percent sign remains literal. See
// https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nn-taskschd-iexecaction.
// Linux executable paths cannot contain quotes, backslashes or glob characters,
// which systemd rejects even after unit unquoting. Other argv paths retain them.
// See https://github.com/systemd/systemd/blob/main/src/core/load-fragment.c.
package brainservice

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/store"
)

type Spec struct{ Name, Executable, ConfigRoot, UserID, UserHome, UserConfigRoot string }
type Registration struct {
	ID, Path   string
	Definition []byte
	Argv       []string
}

var (
	ErrUnsupported      = errors.New("publisher launchers unsupported on this platform")
	ErrInvalidSpec      = errors.New("invalid publisher launcher specification")
	ErrPercentExpansion = errors.New("windows launcher paths cannot contain paired percent signs (Task Scheduler environment expansion)")
	uidPattern          = regexp.MustCompile(`^[1-9][0-9]*$`)
	sidPattern          = regexp.MustCompile(`^S-1-5-21(?:-[0-9]+){4}$`)
)

// Render returns the exact definition and argv for inspection or dry-run. The
// registration key is scoped to the canonical registry root and connection name.
func Render(goos string, spec Spec) (Registration, error) {
	if goos != "linux" && goos != "darwin" && goos != "windows" {
		return Registration{}, ErrUnsupported
	}
	if store.ValidatePushName(spec.Name) != nil {
		return Registration{}, ErrInvalidSpec
	}
	for _, v := range []string{spec.Name, spec.Executable, spec.ConfigRoot, spec.UserID, spec.UserHome, spec.UserConfigRoot} {
		if v == "" || len(v) > 4096 || !utf8.ValidString(v) || strings.IndexFunc(v, func(r rune) bool { return unicode.IsControl(r) || r == 0xfffe || r == 0xffff }) >= 0 || strings.TrimSpace(v) != v {
			return Registration{}, ErrInvalidSpec
		}
	}
	if goos == "windows" {
		if !sidPattern.MatchString(spec.UserID) {
			return Registration{}, ErrInvalidSpec
		}
	} else if !uidPattern.MatchString(spec.UserID) {
		return Registration{}, ErrInvalidSpec
	}
	paths := []*string{&spec.Executable, &spec.ConfigRoot, &spec.UserHome, &spec.UserConfigRoot}
	for _, value := range paths {
		normalized, err := absolutePath(goos, *value)
		if err != nil {
			return Registration{}, err
		}
		*value = normalized
	}
	if goos == "linux" && strings.ContainsAny(spec.Executable, `\"'*?[]`) {
		return Registration{}, ErrInvalidSpec
	}
	sum := sha256.Sum256([]byte(spec.ConfigRoot + "\x00" + spec.Name))
	suffix := hex.EncodeToString(sum[:])
	reg := Registration{Argv: []string{spec.Executable, "brain", "--config-root", spec.ConfigRoot, "run", spec.Name}}
	switch goos {
	case "linux":
		reg.ID = "docbank-brain-" + suffix + ".service"
		reg.Path = path.Join(spec.UserConfigRoot, "systemd", "user", reg.ID)
		args := make([]string, len(reg.Argv))
		for i, arg := range reg.Argv {
			// The : prefix disables environment expansion, including argv[0].
			args[i] = systemdArgument(arg)
		}
		reg.Definition = []byte("[Unit]\nDescription=Docbank Markdown publisher\n\n[Service]\nType=exec\nExecStart=:" + strings.Join(args, " ") + "\nRestart=no\n\n[Install]\nWantedBy=default.target\n")
	case "darwin":
		reg.ID = "io.kenn.docbank.brain." + suffix
		reg.Path = path.Join(spec.UserHome, "Library", "LaunchAgents", reg.ID+".plist")
		var args strings.Builder
		for _, arg := range reg.Argv {
			args.WriteString("<string>" + escapeXML(arg) + "</string>")
		}
		reg.Definition = []byte(xml.Header + `<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n" + `<plist version="1.0"><dict><key>Label</key><string>` + reg.ID + `</string><key>ProgramArguments</key><array>` + args.String() + `</array><key>RunAtLoad</key><true/></dict></plist>` + "\n")
	case "windows":
		reg.ID = "Docbank-Brain-" + suffix
		reg.Path = joinWindows(spec.ConfigRoot, "services", reg.ID+".xml")
		args := make([]string, 0, len(reg.Argv)-1)
		for _, arg := range reg.Argv[1:] {
			args = append(args, windowsArgument(arg))
		}
		reg.Definition = []byte(xml.Header + `<Task version="1.3" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task"><RegistrationInfo><URI>\` + reg.ID + `</URI><Author>` + escapeXML(spec.UserID) + `</Author><Description>Docbank Markdown publisher</Description></RegistrationInfo><Triggers><LogonTrigger><Enabled>true</Enabled><UserId>` + escapeXML(spec.UserID) + `</UserId></LogonTrigger></Triggers><Principals><Principal id="Publisher"><UserId>` + escapeXML(spec.UserID) + `</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals><Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><AllowHardTerminate>true</AllowHardTerminate><StartWhenAvailable>true</StartWhenAvailable><RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable><IdleSettings><StopOnIdleEnd>false</StopOnIdleEnd><RestartOnIdle>false</RestartOnIdle></IdleSettings><AllowStartOnDemand>true</AllowStartOnDemand><Enabled>true</Enabled><Hidden>false</Hidden><RunOnlyIfIdle>false</RunOnlyIfIdle><WakeToRun>false</WakeToRun><ExecutionTimeLimit>PT0S</ExecutionTimeLimit><Priority>7</Priority></Settings><Actions Context="Publisher"><Exec><Command>` + escapeXML(spec.Executable) + `</Command><Arguments>` + escapeXML(strings.Join(args, " ")) + `</Arguments></Exec></Actions></Task>` + "\n")
	}
	return reg, nil
}

func absolutePath(goos, value string) (string, error) {
	if goos != "windows" {
		if !strings.HasPrefix(value, "/") {
			return "", ErrInvalidSpec
		}
		return path.Clean(value), nil
	}
	// There is no escaping convention that suppresses Task Scheduler's percent
	// environment expansion. Reject pairs even when the current variable is unset.
	if strings.Count(value, "%") > 1 {
		return "", ErrPercentExpansion
	}
	value = strings.ReplaceAll(value, "/", `\`)
	var volume, rest string
	if len(value) >= 3 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':' && value[2] == '\\' {
		volume = value[:2]
		rest = value[2:]
	} else if strings.HasPrefix(value, `\\`) {
		parts := strings.SplitN(value[2:], `\`, 3)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return "", ErrInvalidSpec
		}
		volume = `\\` + parts[0] + `\` + parts[1]
		rest = `\`
		if len(parts) == 3 {
			rest += parts[2]
		}
	} else {
		return "", ErrInvalidSpec
	}
	if strings.ContainsAny(rest, `:"<>|?*`) {
		return "", ErrInvalidSpec
	}
	return volume + strings.ReplaceAll(path.Clean(strings.ReplaceAll(rest, `\`, "/")), "/", `\`), nil
}
func joinWindows(root string, parts ...string) string { return root + `\` + strings.Join(parts, `\`) }
func escapeXML(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
func systemdArgument(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`).Replace(s) + `"`
}
func windowsArgument(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, r := range s {
		if r == '\\' {
			slashes++
			continue
		}
		if r == '"' {
			b.WriteString(strings.Repeat(`\`, 2*slashes+1))
		} else {
			b.WriteString(strings.Repeat(`\`, slashes))
		}
		slashes = 0
		b.WriteRune(r)
	}
	b.WriteString(strings.Repeat(`\`, 2*slashes))
	b.WriteByte('"')
	return b.String()
}
