package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

const maxProductionWithheldInputBytes = 64 << 20

var errProductionWithheldOutcomeUnknown = errors.New("production withheld selection write outcome is unknown")

type productionWithheldOutput struct {
	privateCache

	SelectionID  string `json:"selection_id"`
	SetID        string `json:"set_id"`
	Revision     int64  `json:"revision"`
	PolicySHA256 string `json:"policy_sha256"`
	SHA256       string `json:"sha256"`
	MemberCount  int    `json:"member_count"`
}

func readPrivateProductionWithheldFile(path, setID string, revision int64) (
	api.ProductionWithheldSelectionCreateRequest, error) {
	var zero api.ProductionWithheldSelectionCreateRequest
	if len(path) == 0 || len(path) > maxPathCharacters || !filepath.IsAbs(path) ||
		filepath.Clean(path) != path {
		return zero, invalidToolArgumentsError()
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxProductionWithheldInputBytes {
		return zero, invalidToolArgumentsError()
	}
	file, err := os.Open(path)
	if err != nil {
		return zero, invalidToolArgumentsError()
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return zero, invalidToolArgumentsError()
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxProductionWithheldInputBytes+1))
	if err != nil || len(raw) > maxProductionWithheldInputBytes {
		return zero, invalidToolArgumentsError()
	}
	var request api.ProductionWithheldSelectionCreateRequest
	if err := json.Unmarshal(raw, &request, json.RejectUnknownMembers(true)); err != nil ||
		!validToolUUID(request.OperationID) {
		return zero, invalidToolArgumentsError()
	}
	if _, _, err := documentproduction.CanonicalWithheldSelection(documentproduction.WithheldSelection{
		Contract: documentproduction.WithheldSelectionContractV1,
		ID:       request.SelectionID, SetID: setID, Revision: revision,
		PolicySHA256: request.PolicySHA256, Members: request.Members,
	}); err != nil {
		return zero, invalidToolArgumentsError()
	}
	return request, nil
}

func createProductionWithheldSelection(ctx context.Context, lease *daemonLease, raw []byte) (
	productionWithheldOutput, error) {
	if len(raw) > 8192 {
		return productionWithheldOutput{}, invalidToolArgumentsError()
	}
	var input struct {
		SetID         string `json:"set_id"`
		Revision      int64  `json:"revision"`
		SelectionFile string `json:"selection_file"`
	}
	if err := decodeReadArguments(raw, &input); err != nil {
		return productionWithheldOutput{}, err
	}
	if !validToolUUID(input.SetID) || input.Revision < 1 {
		return productionWithheldOutput{}, invalidToolArgumentsError()
	}
	request, err := readPrivateProductionWithheldFile(input.SelectionFile, input.SetID, input.Revision)
	if err != nil {
		return productionWithheldOutput{}, err
	}
	result, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (
		documentproduction.WithheldSelection, error) {
		return c.CreateProductionWithheldSelection(ctx, input.SetID, input.Revision, request)
	})
	if err != nil {
		if errors.Is(err, errProcessingOutcomeUnknown) {
			return productionWithheldOutput{}, errProductionWithheldOutcomeUnknown
		}
		return productionWithheldOutput{}, err
	}
	return productionWithheldOutput{privateCache: newPrivateCache(), SelectionID: result.ID,
		SetID: result.SetID, Revision: result.Revision, PolicySHA256: result.PolicySHA256,
		SHA256: result.SHA256, MemberCount: len(result.Members)}, nil
}

func createProductionWithheldSelectionToolHandler(lease *daemonLease, validator *jsonschema.Resolved,
	logger *slog.Logger) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := createProductionWithheldSelection(ctx, lease, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, createProductionWithheldSelectionToolDefinition.name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}
