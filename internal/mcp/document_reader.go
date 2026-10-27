package mcp

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

type documentClient interface {
	VaultInfo(ctx context.Context) (api.VaultInfo, error)
	ListDocuments(ctx context.Context, request api.DocumentQuery) (api.DocumentPage, error)
	ResolveDocumentSummaries(ctx context.Context, request api.DocumentSummaryResolveRequest) ([]api.DocumentSummary, error)
	ResolveDocumentSourceFence(ctx context.Context, request api.DocumentSourceFenceResolveRequest) (api.DocumentSourceFenceResolution, error)
	ValidateDocumentSearch(ctx context.Context, request api.DocumentSearchValidationRequest) error
	SearchDocuments(ctx context.Context, request api.DocumentSearchRequest) (api.DocumentSearchReport, error)
}

type daemonDocumentClient struct{ *daemonconn.Connection }

func (c daemonDocumentClient) VaultInfo(ctx context.Context) (api.VaultInfo, error) {
	info, err := c.API().VaultInfo(ctx)
	if err != nil {
		return api.VaultInfo{}, err
	}
	return *info, nil
}

// One session wraps the complete multi-read tool operation. The daemon retains
// its existing retry boundary; the embedded session never discovers a daemon.
type documentReadSession func(context.Context, func(context.Context, documentClient) error) error

func daemonDocumentSession(lease *daemonLease) documentReadSession {
	return func(ctx context.Context, call func(context.Context, documentClient) error) error {
		_, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (struct{}, error) {
			return struct{}{}, call(ctx, daemonDocumentClient{c})
		})
		return err
	}
}
func documentRead[T any](ctx context.Context, session documentReadSession, call func(context.Context, documentClient) (T, error)) (T, error) {
	var out T
	err := session(ctx, func(ctx context.Context, c documentClient) error {
		var err error
		out, err = call(ctx, c)
		return err
	})
	return out, err
}

type documentToolSchemas struct{ input, output *jsonschema.Resolved }

var embeddedDocumentSchemas = func() map[string]documentToolSchemas {
	out := map[string]documentToolSchemas{}
	for _, tool := range toolCatalog(ServerOptions{}) {
		switch tool.Name {
		case "get_vault_info", "list_documents", "search_documents", "find_conflicts":
			out[tool.Name] = documentToolSchemas{mustResolveSchema(tool.InputSchema), mustResolveSchema(tool.OutputSchema)}
		}
	}
	return out
}()

// CallDocumentReadTool uses the same schemas, handlers and result bounds as the
// daemon-backed tools. The caller has already admitted access to the whole vault.
func CallDocumentReadTool(ctx context.Context, reader *api.DocumentReader, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
	if req == nil || req.Params == nil {
		return nil, invalidToolArgumentsError()
	}
	schemas, ok := embeddedDocumentSchemas[req.Params.Name]
	if !ok {
		return nil, invalidToolArgumentsError()
	}
	args, err := decodeToolArguments(req.Params.Arguments)
	if err != nil || schemas.input.Validate(&args) != nil || !validToolSemantics(req.Params.Name, args) {
		return nil, invalidToolArgumentsError()
	}
	session := func(ctx context.Context, call func(context.Context, documentClient) error) error {
		return call(ctx, reader)
	}
	var out any
	var links []*sdkmcp.ResourceLink
	switch req.Params.Name {
	case "list_documents":
		out, links, err = listDocuments(ctx, session, req.Params.Arguments)
	case "search_documents":
		out, links, err = searchDocuments(ctx, session, req.Params.Arguments)
	case "find_conflicts":
		out, err = findConflicts(ctx, session, req.Params.Arguments)
	case "get_vault_info":
		out, err = getVaultInfo(ctx, session, req.Params.Arguments)
	}
	if err == nil {
		result, boundErr := boundedToolSuccess(schemas.output, out, links)
		if boundErr == nil {
			return result, nil
		}
		err = boundErr
	}
	return readToolFailure(slog.Default(), req.Params.Name, err)
}

func readToolFailure(logger *slog.Logger, name string, err error) (*sdkmcp.CallToolResult, error) {
	logOperationError(logger, name, err)
	if domain, ok := domainToolError(err); ok {
		return domain, nil
	}
	if invalid, ok := errors.AsType[*jsonrpc.Error](err); ok && invalid.Code == jsonrpc.CodeInvalidParams {
		return nil, invalid
	}
	return nil, sanitizedRPCError(err)
}
