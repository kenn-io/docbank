package main

import (
	"context"
	"fmt"
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
			feature := codexFeatureItem()
			check := item.Check
			item.Check = func(ctx context.Context) (agentsetup.Status, error) {
				status, err := feature.Check(ctx)
				if err == nil && status.State == agentsetup.StateFailed {
					return agentsetup.Status{State: agentsetup.StateFailed, Reason: "needs the Codex feature step above"}, nil
				}
				return check(ctx)
			}
			return []agentsetup.Item{feature, item}, nil
		},
	})
	cmd.Short = "Connect Claude Code and Codex to Docbank"
	cmd.Long = "Register the read-only Docbank MCP server with installed agents."
	cmd.Flags().Lookup("json").Usage = "print JSON to stdout"
	clisearch.SetKeywords(cmd, "connect Claude Code and Codex to docbank")
	cmd.Example = "  docbank agent setup\n  docbank agent setup --agent codex --check"
	return cmd
}

func codexFeatureItem() agentsetup.Item {
	probe := func(ctx context.Context) (agentsetup.Status, bool) {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		output, err := exec.CommandContext(ctx, "codex", "features", "list").Output()
		if err != nil {
			return agentsetup.Status{State: agentsetup.StateUnknown, Reason: fmt.Sprintf("checking Codex features: %v", err), NextStep: codexEnable}, false
		}
		for line := range strings.SplitSeq(string(output), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 || fields[0] != codexFeature {
				continue
			}
			if len(fields) >= 2 {
				switch fields[len(fields)-1] {
				case "true":
					return agentsetup.Status{State: agentsetup.StateOK, Reason: "on; Codex shows an under-development feature notice each session"}, false
				case "false":
					return agentsetup.Status{State: agentsetup.StateFailed, Reason: "Codex's MCP 2026-07-28 support is off", NextStep: codexEnable + " (or docbank agent setup)"}, true
				}
			}
			return agentsetup.Status{State: agentsetup.StateUnknown, Reason: "cannot parse Codex feature state", NextStep: codexEnable}, false
		}
		return agentsetup.Status{State: agentsetup.StateFailed, Reason: "this Codex has no mcp_2026_07_28 feature", NextStep: "upgrade Codex to 0.164.0 or later"}, false
	}
	check := func(ctx context.Context) (agentsetup.Status, error) {
		status, _ := probe(ctx)
		return status, nil
	}
	return agentsetup.Item{
		Name:  codexFeature + " feature",
		Check: check,
		Install: func(ctx context.Context) error {
			_, off := probe(ctx)
			if !off {
				return nil
			}
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, "codex", "features", "enable", codexFeature).CombinedOutput()
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
