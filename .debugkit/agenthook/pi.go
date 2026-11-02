package agenthook

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

//go:embed pi_extension.js
var piExtension string

func piProfile() profileSpec {
	spec := newProfileSpec(
		Profile{
			Agent: AgentPi, DisplayName: "Pi",
			ConfigEnvironment: "PI_CODING_AGENT_DIR",
			// Pi loads top-level *.js and *.ts files from <agent dir>/extensions
			// and skips dotfiles, so atomicfile's staging file is never loaded:
			// https://github.com/earendil-works/pi/blob/e37cdc420669e48b4df4bbce0fb2617fc3c71328/packages/coding-agent/src/core/package-manager.ts#L592-L645
			ConfigFilename: filepath.Join("extensions", "agenthook.js"),
			// SessionEnd fires only when another session replaces this one (new,
			// resume, fork), never on quit or reload, so a stopped Pi stays resumable.
			SupportedEvents: []Event{
				EventSessionStart, EventUserPromptSubmit, EventStop, EventSessionEnd,
			},
		},
		formatScript,
		"",
		func() (string, error) { return userDotDir(filepath.Join(".pi", "agent")) },
	)
	spec.configEnvDir = piAgentDir
	spec.eventName = piEventName
	spec.script = piExtension
	// Pi extension handlers run in-process; the generated extension ignores
	// command output, so control decisions have nowhere to go.
	spec.responseFormat = responseObservational
	// session_start carries a reason that maps to source except for reload, and
	// the extension reports session_shutdown only for reasons that map to one:
	// https://github.com/earendil-works/pi/blob/e37cdc420669e48b4df4bbce0fb2617fc3c71328/packages/coding-agent/src/core/extensions/types.ts#L741-L747
	// https://github.com/earendil-works/pi/blob/e37cdc420669e48b4df4bbce0fb2617fc3c71328/packages/coding-agent/src/core/extensions/types.ts#L810-L815
	spec.sessionSourceRequirement = inputOptional
	spec.sessionEndReasonRequirement = inputRequired
	return spec
}

// piEventName maps Claude events to Pi extension events:
// https://github.com/earendil-works/pi/blob/e37cdc420669e48b4df4bbce0fb2617fc3c71328/packages/coding-agent/src/core/extensions/types.ts#L919-L929
// https://github.com/earendil-works/pi/blob/e37cdc420669e48b4df4bbce0fb2617fc3c71328/packages/coding-agent/src/core/extensions/types.ts#L1005-L1010
func piEventName(event Event) string {
	switch event {
	case EventSessionStart:
		return "session_start"
	case EventUserPromptSubmit:
		return "before_agent_start"
	case EventStop:
		// agent_settled fires once no retry, compaction, or queued turn follows.
		return "agent_settled"
	case EventSessionEnd:
		return "session_shutdown"
	default:
		return string(event)
	}
}

var piFileURLDrive = regexp.MustCompile(`^file:///?([A-Za-z])[:|](/|$)`)

var piWindowsShellPath = regexp.MustCompile(`(?i)^/(?:mnt/|cygdrive/)?([a-z])(?:/(.*))?$`)

// piAgentDir follows Pi's normalizePath for PI_CODING_AGENT_DIR: Git Bash,
// MSYS, Cygwin, and WSL drive paths on Windows, then ~, then file URLs:
// https://github.com/earendil-works/pi/blob/e37cdc420669e48b4df4bbce0fb2617fc3c71328/packages/coding-agent/src/utils/paths.ts#L67-L101
func piAgentDir(dir string) (string, error) {
	if runtime.GOOS == "windows" && strings.HasPrefix(dir, "/") &&
		!strings.HasPrefix(dir, "//") && !strings.Contains(dir, `\`) {
		if match := piWindowsShellPath.FindStringSubmatch(dir); match != nil {
			return strings.ToUpper(match[1]) + `:\` + strings.ReplaceAll(match[2], "/", `\`), nil
		}
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") ||
		(runtime.GOOS == "windows" && strings.HasPrefix(dir, `~\`)) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		rest := ""
		if len(dir) > 2 {
			rest = dir[2:]
		}
		return filepath.Join(home, rest), nil
	}
	if strings.HasPrefix(dir, "file://") {
		return piFileURLPath(dir)
	}
	return dir, nil
}

// piFileURLPath converts a file URL as Node's fileURLToPath does, failing
// where it throws, since Pi then cannot start:
// https://github.com/nodejs/node/blob/v24.0.0/lib/internal/url.js#L1450-L1512
func piFileURLPath(raw string) (string, error) {
	// The WHATWG URL parser Node uses reads a drive letter after file:// as the
	// path's first segment and writes C| as C:, while Go reads C: as a host.
	parsed, err := url.Parse(piFileURLDrive.ReplaceAllString(raw, "file:///$1:$2"))
	if err != nil {
		return "", fmt.Errorf("parse file URL %q: %w", raw, err)
	}
	escaped := strings.ToLower(parsed.EscapedPath())
	if strings.Contains(escaped, "%2f") ||
		(runtime.GOOS == "windows" && strings.Contains(escaped, "%5c")) {
		return "", fmt.Errorf("file URL %q must not encode path separators", raw)
	}
	// WHATWG parsing also lowercases the host and drops localhost.
	host := strings.ToLower(parsed.Host)
	if host == "localhost" {
		host = ""
	}
	if runtime.GOOS != "windows" {
		if host != "" {
			return "", fmt.Errorf("file URL %q names host %q; it must name a local path", raw, host)
		}
		return parsed.Path, nil
	}
	path := filepath.FromSlash(parsed.Path)
	if host != "" {
		return `\\` + host + path, nil
	}
	if len(path) < 3 || path[2] != ':' || path[1]|0x20 < 'a' || path[1]|0x20 > 'z' {
		return "", fmt.Errorf("file URL %q must name an absolute path with a drive letter", raw)
	}
	return path[1:], nil
}

// promotePiReason maps Pi's reason field to Claude's. session_start reasons
// become source (Pi's /new starts a fresh session as Claude's /clear does;
// reload has no equivalent), and session_shutdown reasons for a replaced
// session become SessionEnd reasons. The extension reports no shutdown for
// quit or reload.
func promotePiReason(payload map[string]json.RawMessage) error {
	var event, reason string
	if raw, ok := payload["hook_event_name"]; ok {
		if err := json.Unmarshal(raw, &event); err != nil {
			return fmt.Errorf("field %q must be a string: %w", "hook_event_name", err)
		}
	}
	if raw, ok := payload["reason"]; ok {
		if err := json.Unmarshal(raw, &reason); err != nil {
			return fmt.Errorf("field %q must be a string: %w", "reason", err)
		}
	}
	var field, value string
	switch event {
	case "session_start":
		field = "source"
		value = map[string]string{
			"startup": string(SessionSourceStartup),
			"resume":  string(SessionSourceResume),
			"fork":    string(SessionSourceFork),
			"new":     string(SessionSourceClear),
		}[reason]
	case "session_shutdown":
		field = "reason"
		value = map[string]string{
			"new":    string(SessionEndClear),
			"resume": string(SessionEndResume),
			"fork":   string(SessionEndOther),
		}[reason]
	default:
		return nil
	}
	if value == "" {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	payload[field] = encoded
	return nil
}
