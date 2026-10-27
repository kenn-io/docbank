package mcp

import (
	"context"
	"log/slog"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

var searchFeedbackTool = toolDefinition{
	name: "record_search_feedback", title: "Record search feedback",
	description: "Correct one opted-in search using its ID and feedback capability. Writes private local feedback, with no log reads or exports.",
	schemas:     searchFeedbackSchemas, write: true, idempotent: true,
}

func searchFeedbackSchemas() (schema, schema) {
	feedback := objectSchema(schema{
		"verdict":  enumSchema("wrong", "missing", "low_confidence", "none_in_vault"),
		"expected": arraySchema(objectSchema(schema{"node_id": integerSchema(1, 0), "content_version_id": uuidSchema()}, "node_id"), 20),
		"note":     stringSchema(1000),
	}, "verdict")
	input := rootObjectSchema(schema{"id": schema{"type": "string", "pattern": "^[0-9a-f]{48}$"}, "capability": schema{"type": "string", "pattern": "^[0-9a-f]{64}$"}, "feedback": feedback}, "id", "capability", "feedback")
	output := rootObjectSchema(schema{"id": schema{"type": "string", "pattern": "^[0-9a-f]{48}$"}, "success": schema{"type": "boolean", jsonSchemaConst: true}}, "id", "success")
	return input, output
}
func searchFeedbackHandler(lease *daemonLease, validator *jsonschema.Resolved, logger *slog.Logger) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		var input api.SearchFeedbackRequest
		if err := decodeReadArguments(request.Params.Arguments, &input); err != nil {
			return nil, invalidToolArgumentsError()
		}
		// Mutations use the existing no-retry boundary: a lost response must not
		// replay feedback after another caller has replaced it.
		out, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.SearchFeedbackResult, error) {
			return c.RecordSearchFeedback(ctx, input)
		})
		if err != nil {
			return readToolFailure(logger, searchFeedbackTool.name, err)
		}
		return boundedToolSuccess(validator, out, nil)
	}
}
