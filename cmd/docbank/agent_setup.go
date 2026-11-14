package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/agenthook"
	"go.kenn.io/kit/agentmcp"
	"go.kenn.io/kit/agentsetup"
	"go.kenn.io/kit/clisearch"
)

const codexFeature = "mcp_2026_07_28"
const codexEnable = "codex features enable " + codexFeature

// codexOffStep also covers a trusted project that disables the feature, which codexEnable cannot override.
const codexOffStep = codexEnable + " (or docbank agent setup); if it stays off, set " + codexFeature + " = true under [features] in the trusted project's .codex/config.toml"

func newAgentSetupCommand() *cobra.Command {
	cmd := agentsetup.NewCommand(agentsetup.Options{
		SetupCommand: "docbank agent setup",
		Items: func(agent string) ([]agentsetup.Item, error) {
			server, err := mcpConfigServer(agentmcp.Agent(agent), "stdio", "", nil)
			if err != nil {
				return nil, err
			}
			item := agentsetup.MCP(agent, "docbank", server)
			if agent != "codex" {
				return []agentsetup.Item{item}, nil
			}
			return []agentsetup.Item{codexFeatureItem(), item}, nil
		},
	})
	run := cmd.RunE
	cmd.RunE = func(c *cobra.Command, args []string) error {
		err := run(c, args)
		if err != nil && strings.Contains(err.Error(), "no supported agent") {
			err = withHint(err, "install Claude Code or Codex, or pass --agent")
		}
		return err
	}
	cmd.Flags().Lookup("json").Usage = "print JSON to stdout"
	clisearch.SetKeywords(cmd, "connect Claude Code and Codex to docbank")
	cmd.Example = "  docbank agent setup\n  docbank agent setup --agent codex --check"
	return cmd
}

// codexCommand runs in dir; an empty dir keeps the current project's settings in effect.
func codexCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "codex", args...) //nolint:gosec // Callers supply fixed Codex feature commands.
	cmd.Dir = dir
	return cmd
}

func codexFeatureItem() agentsetup.Item {
	probe := func(ctx context.Context, dir string) (agentsetup.Status, bool) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		output, err := codexCommand(ctx, dir, "features", "list").CombinedOutput()
		if err != nil {
			return agentsetup.Status{State: agentsetup.StateUnknown, Reason: fmt.Sprintf("checking Codex features: %v: %s", err, bytes.TrimSpace(output)), NextStep: "install Codex or fix its setup, then run " + codexEnable}, false
		}
		for line := range strings.SplitSeq(string(output), "\n") {
			if fields := strings.Fields(line); len(fields) > 0 && fields[0] == codexFeature {
				switch fields[len(fields)-1] {
				case "true":
					return agentsetup.Status{State: agentsetup.StateOK, Reason: "on; Codex shows an under-development feature notice each session"}, false
				case "false":
					return agentsetup.Status{State: agentsetup.StateFailed, Reason: "Codex's MCP 2026-07-28 support is off", NextStep: codexOffStep}, true
				}
				return agentsetup.Status{State: agentsetup.StateUnknown, Reason: "cannot parse Codex feature state", NextStep: codexEnable}, false
			}
		}
		return agentsetup.Status{State: agentsetup.StateFailed, Reason: "this Codex has no mcp_2026_07_28 feature", NextStep: "upgrade Codex to 0.164.0 or later"}, false
	}
	return agentsetup.Item{
		Name: codexFeature + " feature",
		Check: func(ctx context.Context) (agentsetup.Status, error) {
			status, _ := probe(ctx, "")
			return status, nil
		},
		Install: func(ctx context.Context) error {
			home, _ := os.UserHomeDir()
			if _, off := probe(ctx, home); !off {
				return nil
			}
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			output, err := codexCommand(ctx, home, "features", "enable", codexFeature).CombinedOutput()
			if err != nil {
				return fmt.Errorf("enabling Codex feature: %w: %s", err, strings.TrimSpace(string(output)))
			}
			return nil
		},
		Commands: func() (agenthook.Commands, error) {
			return agenthook.Commands{POSIX: codexEnable, PowerShell: codexEnable}, nil
		},
	}
}

func init() {
	agent := &cobra.Command{Use: "agent", Short: "Set up agent integrations", Long: "Connect agents to the selected Docbank vault with agent setup.", GroupID: groupInterfaces}
	agent.AddCommand(newAgentSetupCommand())
	rootCmd.AddCommand(agent)
}
