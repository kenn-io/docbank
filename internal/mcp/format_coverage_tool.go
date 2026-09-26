package mcp

import (
	"context"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

type formatCoverageInput struct {
	Family    string `json:"family"`
	Format    string `json:"format"`
	Extension string `json:"extension"`
}

type formatCoverageOutput struct {
	api.FormatCoverageResponse
	privateCache
}

func getFormatCoverage(ctx context.Context, lease *daemonLease, raw []byte) (formatCoverageOutput, error) {
	var input formatCoverageInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return formatCoverageOutput{}, err
	}
	if input.Format != "" && input.Extension != "" || len(input.Family) > 64 ||
		len(input.Format) > 64 || len(input.Extension) > 16 {
		return formatCoverageOutput{}, invalidToolArgumentsError()
	}
	response, err := daemonRead(ctx, lease, func(ctx context.Context, connection *daemonconn.Connection) (api.FormatCoverageResponse, error) {
		return connection.FormatCapabilities(ctx, input.Family, input.Format, input.Extension)
	})
	if err != nil {
		return formatCoverageOutput{}, err
	}
	return formatCoverageOutput{FormatCoverageResponse: response, privateCache: newPrivateCache()}, nil
}
