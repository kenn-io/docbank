package mcp

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/store"
)

var errProductionPolicyOutcomeUnknown = errors.New("production policy write outcome is unknown")

type productionPolicyOutput struct {
	privateCache

	Policy documentproduction.PolicyVersion `json:"policy"`
}

type productionPolicySummary struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
	Name    string `json:"name"`
	SHA256  string `json:"sha256"`
}

type productionPolicyPageOutput struct {
	privateCache

	Items      []productionPolicySummary `json:"items"`
	NextCursor string                    `json:"next_cursor"`
}

func listProductionPolicies(ctx context.Context, lease *daemonLease, raw []byte) (productionPolicyPageOutput, error) {
	var input struct {
		Cursor string `json:"cursor"`
		Limit  int    `json:"limit"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionPolicyPageOutput{}, err
	}
	if len(input.Cursor) > 60 || input.Limit < 0 || input.Limit > store.MaxProductionPolicyPage {
		return productionPolicyPageOutput{}, invalidToolArgumentsError()
	}
	if input.Limit == 0 {
		input.Limit = 25
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.ProductionPolicyPage, error) {
		result, err := c.ProductionPolicyVersions(ctx, input.Cursor, input.Limit)
		return &result, err
	})
	if err != nil {
		return productionPolicyPageOutput{}, err
	}
	items := make([]productionPolicySummary, len(page.Items))
	for i, item := range page.Items {
		items[i] = productionPolicySummary{ID: item.ID, Version: item.Version,
			Name: item.Name, SHA256: item.SHA256}
	}
	return productionPolicyPageOutput{privateCache: newPrivateCache(), Items: items,
		NextCursor: page.NextCursor}, nil
}

func getProductionPolicy(ctx context.Context, lease *daemonLease, raw []byte) (productionPolicyOutput, error) {
	var input struct {
		PolicyID string `json:"policy_id"`
		Version  int64  `json:"version"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionPolicyOutput{}, err
	}
	if !validToolUUID(input.PolicyID) || input.Version < 1 {
		return productionPolicyOutput{}, invalidToolArgumentsError()
	}
	policy, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (documentproduction.PolicyVersion, error) {
		return c.ProductionPolicyVersion(ctx, input.PolicyID, input.Version)
	})
	if err != nil {
		return productionPolicyOutput{}, err
	}
	return productionPolicyOutput{Policy: policy, privateCache: newPrivateCache()}, nil
}

func createProductionPolicy(ctx context.Context, lease *daemonLease, raw []byte) (productionPolicyOutput, error) {
	if len(raw) > 1<<20 {
		return productionPolicyOutput{}, invalidToolArgumentsError()
	}
	var input api.ProductionPolicyCreateRequest
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionPolicyOutput{}, err
	}
	policy, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (documentproduction.PolicyVersion, error) {
		return c.CreateProductionPolicyVersion(ctx, input)
	})
	if err != nil {
		if errors.Is(err, errProcessingOutcomeUnknown) {
			return productionPolicyOutput{}, errProductionPolicyOutcomeUnknown
		}
		return productionPolicyOutput{}, err
	}
	return productionPolicyOutput{Policy: policy, privateCache: newPrivateCache()}, nil
}

func createProductionPolicyToolHandler(
	lease *daemonLease, validator *jsonschema.Resolved, logger *slog.Logger,
) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := createProductionPolicy(ctx, lease, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, createProductionPolicyToolDefinition.name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}
