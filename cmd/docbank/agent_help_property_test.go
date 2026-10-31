//go:build !darwin || arm64

package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"github.com/spf13/cobra"
	"strconv"
	"strings"
	"testing"

	"hegel.dev/go/hegel"
)

// Hegel embeds native engines for Linux/Windows amd64/arm64 and macOS arm64.
// Deterministic process-boundary coverage runs on every supported platform.
func TestHelpInvalidSelectorRecoveryProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		raw := hegel.Draw(ht, hegel.Text())
		selector, err := parseNodeSelector(raw)
		if err == nil {
			if !strings.HasPrefix(raw, "/") && selector.id < 1 {
				ht.Fatalf("accepted invalid selector %q", raw)
			}
			return
		}
		if commandExitCode(err, true) != 2 || !strings.Contains(commandErrorHint(statCmd, err, 2, true), "docbank tree /") {
			ht.Fatalf("invalid selector %q has no recovery: %v", raw, err)
		}
	}, hegel.WithDatabase(t.TempDir()))
}

func TestHelpJSONFinalRecordProperty(t *testing.T) {
	var message, diagnostic string
	probe := processProbe(t, func(cmd *cobra.Command, _ []string) error {
		_, _ = cmd.ErrOrStderr().Write([]byte(diagnostic))
		_, _ = cmd.OutOrStdout().Write([]byte("original\x00bytes\n"))
		return usageError(errors.New(message))
	})
	hegel.Test(t, func(ht *hegel.T) {
		message = hegel.Draw(ht, hegel.Text())
		diagnostic = hegel.Draw(ht, hegel.Text())
		resetFlags(rootCmd)
		var out, diag bytes.Buffer
		code := runProcess([]string{probe.Name(), "--json"}, &out, &diag)
		if code != 2 || out.String() != "original\x00bytes\n" {
			ht.Fatalf("changed stdout or exit: %d %q", code, out.String())
		}
		lines := strings.Split(diag.String(), "\n")
		if len(lines) < 2 || lines[len(lines)-1] != "" {
			ht.Fatalf("missing final newline: %q", diag.String())
		}
		var env processErrorEnvelope
		if err := json.Unmarshal([]byte(lines[len(lines)-2]), &env); err != nil {
			ht.Fatalf("invalid final record after diagnostic %q: %v", diagnostic, err)
		}
		if env.Error.Message != message || env.Error.Exit != 2 || env.Error.Code != "usage" || !strings.Contains(env.Error.Hint, probe.Name()+" --help") {
			ht.Fatalf("wrong error record: %+v", env)
		}
	}, hegel.WithDatabase(t.TempDir()))
}

func TestHelpTransportMessageProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		detail := hegel.Draw(ht, hegel.Text())
		literal := errors.New("error executing request: " + detail)
		wrapped := fmt.Errorf("outer context: %w", fmt.Errorf("error executing request: %w", literal))
		want := "outer context: " + literal.Error()
		if got := cliErrorMessage(wrapped); got != want {
			ht.Fatalf("altered underlying detail: got %q want %q", got, want)
		}
	}, hegel.WithDatabase(t.TempDir()))
}

func TestHelpParsedOutputModeProperty(t *testing.T) {
	probe := processProbe(t, func(*cobra.Command, []string) error { return usageError(errors.New("failed")) })
	hegel.Test(t, func(ht *hegel.T) {
		first := hegel.Draw(ht, hegel.Booleans())
		last := hegel.Draw(ht, hegel.Booleans())
		agent := hegel.Draw(ht, hegel.Booleans())
		separated := hegel.Draw(ht, hegel.Booleans())
		args := []string{probe.Name(), "--json=" + strconv.FormatBool(first), "--agent=" + strconv.FormatBool(agent), "--json=" + strconv.FormatBool(last)}
		wantJSON, wantAgent := last, agent
		if separated {
			args = []string{probe.Name(), "--json=false", "--", "--json=" + strconv.FormatBool(last), "--agent=" + strconv.FormatBool(agent)}
			wantJSON, wantAgent = false, false
		}
		resetFlags(rootCmd)
		var out, diag bytes.Buffer
		if code := runProcess(args, &out, &diag); code != 2 || out.Len() != 0 {
			ht.Fatalf("unexpected error streams: %d %q", code, out.String())
		}
		if wantJSON {
			var env processErrorEnvelope
			if err := json.Unmarshal(diag.Bytes(), &env); err != nil || env.Error.Code != "usage" || env.Error.Exit != 2 {
				ht.Fatalf("wrong JSON selection: %q (%v)", diag.String(), err)
			}
		} else {
			prefix := "hint:"
			if wantAgent {
				prefix = "Hint:"
			}
			if !strings.HasPrefix(diag.String(), "error: failed\n") || !strings.Contains(diag.String(), "\n"+prefix+" ") {
				ht.Fatalf("wrong text selection: %q", diag.String())
			}
		}
	}, hegel.WithDatabase(t.TempDir()))
}

func TestHelpMalformedOutputModesProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		jsonBefore := hegel.Draw(ht, hegel.Booleans())
		agentBefore := hegel.Draw(ht, hegel.Booleans())
		name := hegel.Draw(ht, hegel.SampledFrom([]string{"json", "agent"}))
		invalid := "invalid-" + hegel.Draw(ht, hegel.Text())
		resetFlags(rootCmd)
		var out, diag bytes.Buffer
		args := []string{"search", "--json=" + strconv.FormatBool(jsonBefore), "--agent=" + strconv.FormatBool(agentBefore), "--" + name + "=" + invalid}
		if code := runProcess(args, &out, &diag); code != 2 || out.Len() != 0 {
			ht.Fatalf("malformed mode changed exit/stdout: %d %q", code, out.String())
		}
		if jsonBefore {
			var env processErrorEnvelope
			if err := json.Unmarshal(diag.Bytes(), &env); err != nil || env.Error.Code != "usage" || env.Error.Exit != 2 {
				ht.Fatalf("lost parsed JSON selection: %q (%v)", diag.String(), err)
			}
		} else {
			hint := "hint:"
			if agentBefore {
				hint = "Hint:"
			}
			if !strings.HasPrefix(diag.String(), "error: ") || !strings.Contains(diag.String(), "\n"+hint+" ") {
				ht.Fatalf("lost parsed text selection: %q", diag.String())
			}
		}
	}, hegel.WithDatabase(t.TempDir()))
}
