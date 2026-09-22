package mcp

import (
	"context"
	"errors"
	"log/slog"
	"uuid"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

type listPackageCustodiansInput struct {
	PackageID      string  `json:"package_id"`
	RowID          *string `json:"row_id"`
	UnresolvedOnly bool    `json:"unresolved_only"`
	Cursor         string  `json:"cursor"`
	Limit          int     `json:"limit"`
}

type findPeopleInput struct {
	Query  string `json:"query"`
	Cursor string `json:"cursor"`
	Limit  int    `json:"limit"`
}

type custodianPageOutput struct {
	privateCache
	api.CustodianPage
}

type personPageOutput struct {
	privateCache
	api.PersonPage
}

type custodianOutput struct {
	privateCache
	api.CustodianAssignment
}

func listPackageCustodians(ctx context.Context, lease *daemonLease, raw []byte) (custodianPageOutput, error) {
	var input listPackageCustodiansInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return custodianPageOutput{}, err
	}
	packageID, err := uuid.Parse(input.PackageID)
	if err != nil || input.RowID != nil && *input.RowID == "" {
		return custodianPageOutput{}, invalidToolArgumentsError()
	}
	if input.Limit == 0 {
		input.Limit = 100
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.CustodianPage, error) {
		return c.API().ListPackageCustodians(ctx, &apiclient.ListPackageCustodiansRequestOptions{
			PathParams: &apiclient.ListPackageCustodiansPath{PackageID: packageID},
			Query: &apiclient.ListPackageCustodiansQuery{RowID: input.RowID, UnresolvedOnly: &input.UnresolvedOnly,
				Cursor: optionalString(input.Cursor), Limit: &input.Limit},
		})
	})
	if err != nil {
		return custodianPageOutput{}, err
	}
	if len(page.Items) > input.Limit || page.Total < int64(len(page.Items)) {
		return custodianPageOutput{}, errors.New("custodian page exceeded its requested bound")
	}
	if page.Items == nil {
		page.Items = []api.CustodianAssignment{}
	}
	return custodianPageOutput{CustodianPage: *page, privateCache: newPrivateCache()}, nil
}

func findPeople(ctx context.Context, lease *daemonLease, raw []byte) (personPageOutput, error) {
	var input findPeopleInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return personPageOutput{}, err
	}
	if input.Limit == 0 {
		input.Limit = 100
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (*api.PersonPage, error) {
		return c.API().ListPeople(ctx, &apiclient.ListPeopleRequestOptions{Query: &apiclient.ListPeopleQuery{
			Query: optionalString(input.Query), Cursor: optionalString(input.Cursor), Limit: &input.Limit,
		}})
	})
	if err != nil {
		return personPageOutput{}, err
	}
	if len(page.Items) > input.Limit {
		return personPageOutput{}, errors.New("people page exceeded its requested bound")
	}
	if page.Items == nil {
		page.Items = []api.PersonSummary{}
	}
	return personPageOutput{PersonPage: *page, privateCache: newPrivateCache()}, nil
}

type resolvePackageCustodianInput struct {
	AssignmentID    string `json:"assignment_id"`
	PersonID        string `json:"person_id"`
	IfMatchRevision int64  `json:"if_match_revision"`
}

type assignPackageCustodianInput struct {
	PackageID       string  `json:"package_id"`
	RowID           *string `json:"row_id"`
	RawLabel        string  `json:"raw_label"`
	PersonID        string  `json:"person_id"`
	IfMatchRevision int64   `json:"if_match_revision"`
}

func packageCustodianWriteToolHandler(
	lease *daemonLease, name string, validator *jsonschema.Resolved, logger *slog.Logger,
) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		var output custodianOutput
		var err error
		switch name {
		case resolvePackageCustodianToolDefinition.name:
			output, err = resolvePackageCustodian(ctx, lease, request.Params.Arguments)
		case assignPackageCustodianToolDefinition.name:
			output, err = assignPackageCustodian(ctx, lease, request.Params.Arguments)
		default:
			err = errors.New("unknown package custodian write tool")
		}
		if err != nil {
			logOperationError(logger, name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return boundedToolSuccess(validator, output, nil)
	}
}

func resolvePackageCustodian(ctx context.Context, lease *daemonLease, raw []byte) (custodianOutput, error) {
	var input resolvePackageCustodianInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return custodianOutput{}, err
	}
	assignmentID, err := uuid.Parse(input.AssignmentID)
	if err != nil {
		return custodianOutput{}, invalidToolArgumentsError()
	}
	result, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (*api.CustodianAssignment, error) {
		return c.API().ResolvePackageCustodian(ctx, &apiclient.ResolvePackageCustodianRequestOptions{
			PathParams: &apiclient.ResolvePackageCustodianPath{AssignmentID: assignmentID},
			Body:       &api.CustodianResolveRequest{PersonID: input.PersonID, IfMatchRevision: input.IfMatchRevision},
		})
	})
	if err != nil {
		return custodianOutput{}, err
	}
	if result.AssignmentID != input.AssignmentID || result.PersonID == "" || result.Revision != input.IfMatchRevision+1 {
		return custodianOutput{}, errors.New("custodian resolution response does not bind its exact mutation")
	}
	return custodianOutput{CustodianAssignment: *result, privateCache: newPrivateCache()}, nil
}

func assignPackageCustodian(ctx context.Context, lease *daemonLease, raw []byte) (custodianOutput, error) {
	var input assignPackageCustodianInput
	if err := decodeReadArguments(raw, &input); err != nil {
		return custodianOutput{}, err
	}
	packageID, err := uuid.Parse(input.PackageID)
	if err != nil || input.RowID != nil && *input.RowID == "" {
		return custodianOutput{}, invalidToolArgumentsError()
	}
	result, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (*api.CustodianAssignment, error) {
		return c.API().AssignPackageCustodian(ctx, &apiclient.AssignPackageCustodianRequestOptions{
			PathParams: &apiclient.AssignPackageCustodianPath{PackageID: packageID},
			Body: &api.PackageCustodianRequest{RowID: input.RowID, RawLabel: input.RawLabel,
				PersonID: input.PersonID, IfMatchRevision: input.IfMatchRevision},
		})
	})
	if err != nil {
		return custodianOutput{}, err
	}
	if result.PackageID != input.PackageID || result.RawLabel != input.RawLabel || result.Basis != "operator_assigned" ||
		result.Revision < input.IfMatchRevision {
		return custodianOutput{}, errors.New("custodian assignment response does not bind its exact mutation")
	}
	return custodianOutput{CustodianAssignment: *result, privateCache: newPrivateCache()}, nil
}
