package agentmcp

import (
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"strings"
)

// Transport selects an MCP connection protocol.
type Transport string

const (
	TransportStdio Transport = "stdio"
	TransportHTTP  Transport = "http"
	TransportSSE   Transport = "sse"
)

// Server describes a stdio command or remote endpoint. With no Transport,
// URL selects streamable HTTP and Command selects stdio. Use TransportSSE
// explicitly for SSE endpoints; this package never guesses from a URL suffix.
// Command, Args, and Env belong to stdio; URL and Headers belong to HTTP.
type Server struct {
	Transport Transport
	Command   string
	Args      []string
	Env       map[string]string
	URL       string
	Headers   map[string]string
	// BearerTokenEnvVar references a client environment variable for Claude or
	// Codex remote servers. It cannot accompany an Authorization header.
	// Names must match [A-Za-z_][A-Za-z0-9_]*.
	BearerTokenEnvVar string
}

var bearerEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("MCP URL must be an absolute HTTP or HTTPS endpoint")
	}
	return nil
}

func (server Server) native(agent Agent) (map[string]any, error) {
	entry := map[string]any{}
	transport := server.Transport
	if transport == "" {
		transport = TransportStdio
		if server.URL != "" {
			transport = TransportHTTP
		}
	}
	if transport != TransportStdio && transport != TransportHTTP && transport != TransportSSE {
		return nil, fmt.Errorf("unsupported MCP transport %q", transport)
	}
	if transport == TransportSSE && agent == AgentCodex {
		return nil, errors.New("the Codex profile requires streamable HTTP or stdio")
	}
	if server.BearerTokenEnvVar != "" {
		if !bearerEnvName.MatchString(server.BearerTokenEnvVar) {
			return nil, errors.New("MCP bearer token environment variable must be a valid identifier")
		}
		if transport == TransportStdio {
			return nil, errors.New("MCP stdio server cannot include a bearer token environment variable")
		}
		if agent != AgentClaude && agent != AgentCodex {
			return nil, fmt.Errorf("bearer token environment references are unsupported for MCP agent %q", agent)
		}
		for header := range server.Headers {
			if strings.EqualFold(header, "Authorization") {
				return nil, errors.New("MCP bearer token environment variable conflicts with Authorization header")
			}
		}
	}
	if server.URL != "" {
		if transport == TransportStdio {
			return nil, errors.New("MCP stdio server cannot include a URL")
		}
		if server.Command != "" || len(server.Args) != 0 || len(server.Env) != 0 {
			return nil, errors.New("MCP HTTP server cannot include stdio fields")
		}
		if err := validateURL(server.URL); err != nil {
			return nil, err
		}
		urlKey, headerKey := "url", "headers"
		if (agent == AgentGemini || agent == AgentQwen) && transport == TransportHTTP {
			urlKey = "httpUrl"
		}
		if agent == AgentCodex {
			headerKey = "http_headers"
		}
		entry[urlKey] = server.URL
		if len(server.Headers) != 0 {
			entry[headerKey] = server.Headers
		}
		if server.BearerTokenEnvVar != "" {
			if agent == AgentCodex {
				entry["bearer_token_env_var"] = server.BearerTokenEnvVar
			} else {
				headers := maps.Clone(server.Headers)
				if headers == nil {
					headers = map[string]string{}
				}
				headers["Authorization"] = "Bearer ${" + server.BearerTokenEnvVar + "}"
				entry[headerKey] = headers
			}
		}
		if agent == AgentClaude || agent == AgentDroid || agent == AgentCopilot {
			entry["type"] = string(transport)
		}
		if agent == AgentHermes && transport == TransportSSE {
			entry["transport"] = "sse"
		}
	} else {
		if transport != TransportStdio {
			return nil, errors.New("MCP remote server requires a URL")
		}
		if strings.TrimSpace(server.Command) == "" {
			return nil, errors.New("MCP server requires a command or URL")
		}
		if len(server.Headers) != 0 {
			return nil, errors.New("MCP stdio server cannot include HTTP headers")
		}
		entry["command"] = server.Command
		if len(server.Args) != 0 {
			entry["args"] = server.Args
		}
		if len(server.Env) != 0 {
			entry["env"] = server.Env
		}
		if agent == AgentClaude || agent == AgentDroid {
			entry["type"] = "stdio"
		}
		if agent == AgentCopilot {
			entry["type"] = "local"
		}
	}
	if agent == AgentCopilot {
		entry["tools"] = []string{"*"}
	}
	return entry, nil
}
