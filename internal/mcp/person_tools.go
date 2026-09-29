package mcp

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"uuid"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

type personToolOutput struct {
	privateCache
	api.Person
}

type personDetailToolOutput struct {
	privateCache
	api.PersonDetail
}

type personMergeToolOutput struct {
	privateCache
	api.PersonMergeReceipt
}

type personSplitToolOutput struct {
	privateCache
	api.PersonSplitReceipt
}

type personArguments map[string]jsontext.Value

func decodePersonArguments(raw []byte) (personArguments, error) {
	arguments := personArguments{}
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return nil, invalidToolArgumentsError()
	}
	return arguments, nil
}

func personArgumentString(arguments personArguments, key string) (string, error) {
	var value string
	if err := json.Unmarshal(arguments[key], &value); err != nil || value == "" {
		return "", invalidToolArgumentsError()
	}
	return value, nil
}

func personArgumentInt64(arguments personArguments, key string) (int64, error) {
	var value int64
	if err := json.Unmarshal(arguments[key], &value); err != nil || value < 1 {
		return 0, invalidToolArgumentsError()
	}
	return value, nil
}

func personArgumentStrings(arguments personArguments, key string) ([]string, error) {
	if len(arguments[key]) == 0 {
		return nil, nil
	}
	var value []string
	if err := json.Unmarshal(arguments[key], &value); err != nil {
		return nil, invalidToolArgumentsError()
	}
	return value, nil
}

func personArgumentExternal(arguments personArguments) ([]api.PersonExternalUID, error) {
	if len(arguments["external_identities"]) == 0 {
		return nil, nil
	}
	var value []api.PersonExternalUID
	if err := json.Unmarshal(arguments["external_identities"], &value, json.RejectUnknownMembers(true)); err != nil {
		return nil, invalidToolArgumentsError()
	}
	return value, nil
}

func personWriteTool(name string) bool {
	switch name {
	case "create_person", "rename_person", "retire_person", "merge_people", "split_person":
		return true
	default:
		return false
	}
}

func getPerson(ctx context.Context, lease *daemonLease, raw []byte) (personDetailToolOutput, error) {
	arguments, err := decodePersonArguments(raw)
	if err != nil {
		return personDetailToolOutput{}, err
	}
	id, err := personArgumentString(arguments, "person_id")
	if err != nil {
		return personDetailToolOutput{}, err
	}
	if _, err := uuid.Parse(id); err != nil {
		return personDetailToolOutput{}, invalidToolArgumentsError()
	}
	detail, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.PersonDetail, error) {
		return c.Person(ctx, id)
	})
	if err != nil {
		return personDetailToolOutput{}, err
	}
	return personDetailToolOutput{privateCache: newPrivateCache(), PersonDetail: detail}, nil
}

func listPersonCustodians(ctx context.Context, lease *daemonLease, raw []byte) (custodianPageOutput, error) {
	arguments, err := decodePersonArguments(raw)
	if err != nil {
		return custodianPageOutput{}, err
	}
	id, err := personArgumentString(arguments, "person_id")
	if err != nil {
		return custodianPageOutput{}, err
	}
	if _, err := uuid.Parse(id); err != nil {
		return custodianPageOutput{}, invalidToolArgumentsError()
	}
	cursor := ""
	if value, ok := arguments["cursor"]; ok {
		if err := json.Unmarshal(value, &cursor); err != nil {
			return custodianPageOutput{}, invalidToolArgumentsError()
		}
	}
	limit := int64(100)
	if value, ok := arguments["limit"]; ok {
		if err := json.Unmarshal(value, &limit); err != nil || limit < 1 || limit > 250 {
			return custodianPageOutput{}, invalidToolArgumentsError()
		}
	}
	page, err := daemonRead(ctx, lease, func(ctx context.Context, c *daemonconn.Connection) (api.CustodianPage, error) {
		return c.PersonCustodians(ctx, id, cursor, int(limit))
	})
	if err != nil {
		return custodianPageOutput{}, err
	}
	if page.Total < int64(len(page.Items)) {
		return custodianPageOutput{}, errors.New("person custodian page exceeded its requested bound")
	}
	return custodianPageOutput{CustodianPage: page, privateCache: newPrivateCache()}, nil
}

func personWriteToolHandler(
	lease *daemonLease, name string, validator *jsonschema.Resolved, logger *slog.Logger,
) sdkmcp.ToolHandler {
	return func(ctx context.Context, request *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		if request == nil || request.Params == nil {
			return nil, invalidToolArgumentsError()
		}
		output, err := executePersonWriteTool(ctx, lease, name, validator, request.Params.Arguments)
		if err != nil {
			logOperationError(logger, name, err)
			if domain, ok := domainToolError(err); ok {
				return domain, nil
			}
			return nil, sanitizedRPCError(err)
		}
		return output, nil
	}
}

func executePersonWriteTool(
	ctx context.Context, lease *daemonLease, name string, validator *jsonschema.Resolved, raw []byte,
) (*sdkmcp.CallToolResult, error) {
	if err := contextCancellation(ctx, nil); err != nil {
		return nil, err
	}
	arguments, err := decodePersonArguments(raw)
	if err != nil {
		return nil, err
	}
	var output any
	switch name {
	case "create_person":
		displayName, err := personArgumentString(arguments, "display_name")
		if err != nil {
			return nil, err
		}
		person, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.Person, error) {
			return c.CreatePerson(ctx, displayName)
		})
		if err != nil {
			return nil, err
		}
		output = personToolOutput{privateCache: newPrivateCache(), Person: person}
	case "rename_person":
		id, err := personArgumentString(arguments, "person_id")
		if err != nil {
			return nil, err
		}
		revision, err := personArgumentInt64(arguments, "if_match_revision")
		if err != nil {
			return nil, err
		}
		displayName, err := personArgumentString(arguments, "display_name")
		if err != nil {
			return nil, err
		}
		person, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.Person, error) {
			return c.RenamePerson(ctx, id, revision, displayName)
		})
		if err != nil {
			return nil, err
		}
		output = personToolOutput{privateCache: newPrivateCache(), Person: person}
	case "retire_person":
		id, err := personArgumentString(arguments, "person_id")
		if err != nil {
			return nil, err
		}
		revision, err := personArgumentInt64(arguments, "if_match_revision")
		if err != nil {
			return nil, err
		}
		person, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.Person, error) {
			return c.RetirePerson(ctx, id, revision)
		})
		if err != nil {
			return nil, err
		}
		output = personToolOutput{privateCache: newPrivateCache(), Person: person}
	case "merge_people":
		survivorID, err := personArgumentString(arguments, "survivor_person_id")
		if err != nil {
			return nil, err
		}
		survivorRevision, err := personArgumentInt64(arguments, "survivor_revision")
		if err != nil {
			return nil, err
		}
		absorbedID, err := personArgumentString(arguments, "absorbed_person_id")
		if err != nil {
			return nil, err
		}
		absorbedRevision, err := personArgumentInt64(arguments, "absorbed_revision")
		if err != nil {
			return nil, err
		}
		operationID, err := personArgumentString(arguments, "operation_id")
		if err != nil {
			return nil, err
		}
		receipt, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.PersonMergeReceipt, error) {
			return c.MergePerson(ctx, survivorID, survivorRevision, absorbedID, absorbedRevision, operationID)
		})
		if err != nil {
			return nil, err
		}
		output = personMergeToolOutput{privateCache: newPrivateCache(), PersonMergeReceipt: receipt}
	case "split_person":
		id, err := personArgumentString(arguments, "person_id")
		if err != nil {
			return nil, err
		}
		revision, err := personArgumentInt64(arguments, "if_match_revision")
		if err != nil {
			return nil, err
		}
		operationID, err := personArgumentString(arguments, "operation_id")
		if err != nil {
			return nil, err
		}
		displayName, err := personArgumentString(arguments, "display_name")
		if err != nil {
			return nil, err
		}
		identityIDs, err := personArgumentStrings(arguments, "identity_ids")
		if err != nil {
			return nil, err
		}
		assignmentIDs, err := personArgumentStrings(arguments, "assignment_ids")
		if err != nil {
			return nil, err
		}
		external, err := personArgumentExternal(arguments)
		if err != nil {
			return nil, err
		}
		receipt, err := daemonProcessingStart(ctx, lease, func(c *daemonconn.Connection) (api.PersonSplitReceipt, error) {
			return c.SplitPerson(ctx, id, revision, api.SplitPersonRequest{OperationID: operationID, DisplayName: displayName,
				IdentityIDs: identityIDs, AssignmentIDs: assignmentIDs, ExternalIdentities: external})
		})
		if err != nil {
			return nil, err
		}
		output = personSplitToolOutput{privateCache: newPrivateCache(), PersonSplitReceipt: receipt}
	default:
		return nil, errors.New("unknown Docbank person write tool")
	}
	result, err := boundedToolSuccess(validator, output, nil)
	if err != nil {
		return nil, sanitizedDaemonError(errProcessingOutcomeUnknown,
			fmt.Errorf("person mutation response failed output validation: %w", err))
	}
	return result, nil
}
