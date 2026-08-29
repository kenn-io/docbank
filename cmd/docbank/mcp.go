package main

import (
	"errors"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	kitlogging "go.kenn.io/kit/logging"

	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/home"
	docmcp "go.kenn.io/docbank/internal/mcp"
)

var (
	mcpTransport       string
	mcpListen          string
	mcpAllowProcessing bool
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Serve the local vault over Model Context Protocol",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runMCP(cmd)
	},
}

func runMCP(cmd *cobra.Command) (retErr error) {
	if err := validateMCPCommandOptions(mcpTransport, mcpListen); err != nil {
		return err
	}
	logger, loggingResult, err := kitlogging.NewLogger(kitlogging.Options{
		Stderr: cmd.ErrOrStderr(), EnvLevelVar: "DOCBANK_LOG_LEVEL",
	})
	if err != nil {
		return errors.New("building MCP diagnostics logger")
	}
	defer func() { retErr = errors.Join(retErr, loggingResult.Close()) }()

	server := docmcp.NewServerWithOptions(docmcp.ServerOptions{
		AllowProcessing: mcpAllowProcessing,
	})
	switch mcpTransport {
	case "stdio":
		return docmcp.ServeStdio(cmd.Context(), server,
			mcpReadCloser(cmd.InOrStdin()), cmd.OutOrStdout(), logger)
	case "http":
		layout, err := home.Resolve()
		if err != nil {
			return err
		}
		token, err := resolveMCPHTTPBearer(layout.Root)
		if err != nil {
			return err
		}
		return docmcp.ServeHTTP(cmd.Context(), server, mcpListen, docmcp.HTTPOptions{
			BearerToken: token, JSONResponse: true, Logger: logger,
		})
	default:
		panic("validated MCP transport became invalid")
	}
}

func validateMCPCommandOptions(transport, listen string) error {
	switch transport {
	case "stdio":
		if listen != "" {
			return errors.New("--listen is only valid with --transport http")
		}
		return nil
	case "http":
		if listen == "" {
			return errors.New("--listen is required with --transport http")
		}
		return docmcp.ValidateHTTPListenAddress(listen)
	default:
		return errors.New("--transport must be stdio or http")
	}
}

func resolveMCPHTTPBearer(root string) (string, error) {
	cfg, err := config.Load(root)
	if err != nil {
		return "", err
	}
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	reference := cfg.MCP.HTTP.CredentialBinding
	if reference == "" {
		return "", errors.New("[mcp.http] credential_binding is required for HTTP")
	}
	name := strings.TrimPrefix(reference, "credential:")
	binding, ok := cfg.CredentialBindings[name]
	if !ok {
		return "", errors.New("MCP HTTP credential binding is not configured")
	}
	token, ok := os.LookupEnv(binding.EnvironmentVariable)
	if !ok || !validMCPHTTPBearer(token) {
		return "", errors.New("MCP HTTP credential is unavailable")
	}
	return token, nil
}

func validMCPHTTPBearer(value string) bool {
	if value == "" || len(value) > 4096 {
		return false
	}
	for _, b := range []byte(value) {
		if b <= ' ' || b == 0x7f {
			return false
		}
	}
	return true
}

type nonClosingReader struct{ io.Reader }

func (nonClosingReader) Close() error { return nil }

func mcpReadCloser(reader io.Reader) io.ReadCloser {
	if closer, ok := reader.(io.ReadCloser); ok {
		return closer
	}
	return nonClosingReader{Reader: reader}
}

func init() {
	mcpCmd.Flags().StringVar(&mcpTransport, "transport", "stdio", "transport: stdio or http")
	mcpCmd.Flags().StringVar(&mcpListen, "listen", "", "explicit loopback IP and port for HTTP")
	mcpCmd.Flags().BoolVar(&mcpAllowProcessing, "allow-processing", false,
		"expose guarded start_processing (still requires prior operator consent)")
	rootCmd.AddCommand(mcpCmd)
}
