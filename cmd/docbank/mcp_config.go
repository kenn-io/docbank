package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/kit/agentmcp"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/home"
	docmcp "go.kenn.io/docbank/internal/mcp"
)

func newMCPConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Print MCP client configuration",
		Long:  "Print client configuration without editing client files.\nCodex 0.164.0 requires: codex --enable mcp_2026_07_28",
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
		server, err := mcpConfigServer(cmd, agent)
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

func mcpConfigServer(cmd *cobra.Command, agent agentmcp.Agent) (agentmcp.Server, error) {
	if err := validateMCPCommandOptions(mcpTransport, mcpListen); err != nil {
		return agentmcp.Server{}, err
	}
	layout, err := home.Resolve()
	if err != nil {
		return agentmcp.Server{}, err
	}
	args := []string{"mcp", "--transport", "stdio"}
	cmd.InheritedFlags().VisitAll(func(flag *pflag.Flag) {
		if strings.HasPrefix(flag.Name, "allow-") && flag.Value.String() == "true" {
			args = append(args, "--"+flag.Name)
		}
	})
	if mcpTransport == "http" && len(args) > 3 {
		return agentmcp.Server{}, errors.New("--allow-* flags belong on the HTTP listener's startup command")
	}
	if mcpTransport == "http" {
		host, port, _ := net.SplitHostPort(mcpListen)
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
		variable, err := mcpHTTPCredentialVariable(cfg)
		if errors.Is(err, errMCPHTTPCredentialUnset) {
			variable = "DOCBANK_MCP_HTTP_TOKEN"
		} else if err != nil {
			return agentmcp.Server{}, err
		}
		return agentmcp.Server{Transport: agentmcp.TransportHTTP, URL: "http://" + net.JoinHostPort(host, port) + mcpHTTPPath, BearerTokenEnvVar: variable}, nil
	}
	command, err := exec.LookPath(os.Args[0])
	if err != nil && !errors.Is(err, exec.ErrDot) {
		return agentmcp.Server{}, fmt.Errorf("resolving Docbank executable: %w", err)
	}
	command, err = filepath.Abs(command)
	if err != nil {
		return agentmcp.Server{}, fmt.Errorf("resolving Docbank launch path: %w", err)
	}
	env := map[string]string{"DOCBANK_HOME": layout.Root}
	if agent == agentmcp.AgentCodex {
		env["CODEX_MCP_PROTOCOL_VERSION"] = docmcp.ProtocolVersion
	}
	return agentmcp.Server{Command: command, Args: args, Env: env}, nil
}

func init() {
	mcpCmd.AddCommand(newMCPConfigCommand())
}
