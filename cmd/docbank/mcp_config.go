package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/kit/agentmcp"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/home"
)

func newMCPConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Print MCP client configuration",
		Example: `  docbank mcp config --client claude-code
  docbank mcp config --client codex --allow-processing`,
		Args: cobra.NoArgs,
	}
	var client string
	cmd.Flags().StringVar(&client, "client", "", "claude-code or codex (required)")
	_ = cmd.MarkFlagRequired("client")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		agent := agentmcp.AgentClaude
		switch client {
		case "claude-code":
		case "codex":
			agent = agentmcp.AgentCodex
		default:
			return errors.New("--client must be claude-code or codex")
		}
		server, err := mcpConfigServer(cmd)
		if err != nil {
			return err
		}
		data, err := agentmcp.MarshalConfig(agent, "docbank", server)
		if err != nil {
			return fmt.Errorf("rendering MCP configuration: %w", err)
		}
		if _, err := cmd.OutOrStdout().Write(data); err != nil {
			return fmt.Errorf("writing MCP configuration: %w", err)
		}
		return nil
	}
	return cmd
}

func mcpConfigServer(cmd *cobra.Command) (agentmcp.Server, error) {
	if err := validateMCPCommandOptions(mcpTransport, mcpListen); err != nil {
		return agentmcp.Server{}, err
	}
	layout, err := home.Resolve()
	if err != nil {
		return agentmcp.Server{}, err
	}
	args := []string{"mcp", "--transport", "stdio"}
	for _, name := range []string{"allow-processing", "allow-package-writes", "allow-photo-edits", "allow-export-writes", "allow-report-writes", "allow-kata-writes"} {
		enabled, err := cmd.Flags().GetBool(name)
		if err != nil {
			return agentmcp.Server{}, fmt.Errorf("reading --%s: %w", name, err)
		}
		if enabled {
			if mcpTransport == "http" {
				return agentmcp.Server{}, errors.New("--allow-* flags belong on the HTTP listener's startup command")
			}
			args = append(args, "--"+name)
		}
	}
	if mcpTransport == "http" {
		host, port, err := net.SplitHostPort(mcpListen)
		if err != nil {
			return agentmcp.Server{}, fmt.Errorf("parsing --listen: %w", err)
		}
		if strings.TrimLeft(port, "0") == "" {
			return agentmcp.Server{}, errors.New("--listen requires a nonzero port for client configuration")
		}
		if ip := net.ParseIP(host); ip.IsUnspecified() {
			host = "::1"
			if ip.To4() != nil {
				host = "127.0.0.1"
			}
		}
		cfg, err := config.LoadMCPHTTP(layout.Root)
		if err != nil {
			return agentmcp.Server{}, err
		}
		if cfg.MCP.HTTP.CredentialBinding == "" {
			return agentmcp.Server{}, errMCPHTTPCredentialUnset
		}
		binding := cfg.CredentialBindings[strings.TrimPrefix(cfg.MCP.HTTP.CredentialBinding, "credential:")]
		return agentmcp.Server{Transport: agentmcp.TransportHTTP, URL: "http://" + net.JoinHostPort(host, port) + "/mcp", BearerTokenEnvVar: binding.EnvironmentVariable}, nil
	}
	command, err := os.Executable()
	if err != nil {
		return agentmcp.Server{}, fmt.Errorf("resolving Docbank executable: %w", err)
	}
	return agentmcp.Server{Command: command, Args: args, Env: map[string]string{"DOCBANK_HOME": layout.Root}}, nil
}

func init() {
	mcpCmd.AddCommand(newMCPConfigCommand())
}
